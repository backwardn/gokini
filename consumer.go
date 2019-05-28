package gokini

import (
	"errors"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/client"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/aws/aws-sdk-go/service/kinesis/kinesisiface"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

const (
	defaultEmptyRecordBackoffMs = 500
	defaultSecondsBackoffClaim  = 30
	defaultEventLoopSleepMs     = 1000
	// ErrCodeKMSThrottlingException is defined in the API Reference https://docs.aws.amazon.com/sdk-for-go/api/service/kinesis/#Kinesis.GetRecords
	// But it's not a constant?
	ErrCodeKMSThrottlingException = "KMSThrottlingException"
)

// RecordConsumer is the interface consumers will implement
type RecordConsumer interface {
	Init(string) error
	ProcessRecords([]*Records, *KinesisConsumer)
	Shutdown()
}

// Records is structure for Kinesis Records
type Records struct {
	Data           []byte `json:"data"`
	PartitionKey   string `json:"partitionKey"`
	SequenceNumber string `json:"sequenceNumber"`
}

type shardStatus struct {
	ID              string
	Checkpoint      string
	AssignedTo      string
	LeaseTimeout    time.Time
	ParentShardID   *string
	Closed          bool
	readyToBeClosed bool
	sync.Mutex
}

// KinesisConsumer contains all the configuration and functions necessary to start the Kinesis Consumer
type KinesisConsumer struct {
	StreamName                  string
	ShardIteratorType           string
	RecordConsumer              RecordConsumer
	EmptyRecordBackoffMs        int
	LeaseDuration               int
	Monitoring                  MonitoringConfiguration
	DisableAutomaticCheckpoints bool
	Retries                     *int
	IgnoreShardOrdering         bool
	TableName                   string
	DynamoReadCapacityUnits     *int64
	DynamoWriteCapacityUnits    *int64
	DynamoBillingMode           *string
	secondsBackoffClaim         int
	eventLoopSleepMs            int
	svc                         kinesisiface.KinesisAPI
	checkpointer                Checkpointer
	stop                        *chan struct{}
	shardStatus                 map[string]*shardStatus
	consumerID                  string
	sigs                        *chan os.Signal
	mService                    monitoringService
	sync.WaitGroup
}

var defaultRetries = 5

// StartConsumer starts the RecordConsumer, calls Init and starts sending records to ProcessRecords
func (kc *KinesisConsumer) StartConsumer() error {
	// Set Defaults
	if kc.EmptyRecordBackoffMs == 0 {
		kc.EmptyRecordBackoffMs = defaultEmptyRecordBackoffMs
	}

	kc.consumerID = uuid.New().String()

	err := kc.Monitoring.init(kc.StreamName, kc.consumerID)
	if err != nil {
		log.Errorf("Failed to start monitoring service: %s", err)
	}
	kc.mService = kc.Monitoring.service

	if kc.secondsBackoffClaim == 0 {
		kc.secondsBackoffClaim = defaultSecondsBackoffClaim
	}

	if kc.eventLoopSleepMs == 0 {
		kc.eventLoopSleepMs = defaultEventLoopSleepMs
	}

	if kc.svc == nil && kc.checkpointer == nil {
		retries := defaultRetries
		if kc.Retries != nil {
			retries = *kc.Retries
		}

		log.Debugf("Creating Kinesis Session")
		session, err := session.NewSessionWithOptions(
			session.Options{
				Config:            aws.Config{Retryer: client.DefaultRetryer{NumMaxRetries: retries}},
				SharedConfigState: session.SharedConfigEnable,
			},
		)
		if err != nil {
			return err
		}
		if endpoint := os.Getenv("KINESIS_ENDPOINT"); endpoint != "" {
			session.Config.Endpoint = aws.String(endpoint)
		}
		kc.svc = kinesis.New(session)
		kc.checkpointer = &DynamoCheckpoint{
			ReadCapacityUnits:  kc.DynamoReadCapacityUnits,
			WriteCapacityUnits: kc.DynamoWriteCapacityUnits,
			BillingMode:        kc.DynamoBillingMode,
			TableName:          kc.TableName,
			Retries:            retries,
			LeaseDuration:      kc.LeaseDuration,
		}
	}

	log.Debugf("Initializing Checkpointer")
	if err := kc.checkpointer.Init(); err != nil {
		log.Fatalf("Failed to start Checkpointer: %s", err)
	}

	kc.shardStatus = make(map[string]*shardStatus)

	sigs := make(chan os.Signal, 1)
	kc.sigs = &sigs
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	stopChan := make(chan struct{})
	kc.stop = &stopChan

	err = kc.getShardIDs("")
	if err != nil {
		log.Errorf("Error getting Kinesis shards: %s", err)
		return err
	}
	go kc.eventLoop()

	return nil
}

func (kc *KinesisConsumer) eventLoop() {
	for {
		log.Debug("Getting shards")
		err := kc.getShardIDs("")
		if err != nil {
			log.Errorf("Error getting Kinesis shards: %s", err)
			// Back-off?
			time.Sleep(500 * time.Millisecond)
		}
		log.Debugf("Found %d shards", len(kc.shardStatus))

		for _, shard := range kc.shardStatus {
			// We already own this shard so carry on
			if shard.AssignedTo == kc.consumerID {
				continue
			}

			err := kc.checkpointer.FetchCheckpoint(shard)
			if err != nil {
				if err != ErrSequenceIDNotFound {
					log.Error(err)
					continue
				}
			}

			err = kc.checkpointer.GetLease(shard, kc.consumerID)
			if err != nil {
				if err.Error() != ErrLeaseNotAquired {
					log.Error(err)
				}
				continue
			}

			kc.mService.leaseGained(shard.ID)

			kc.RecordConsumer.Init(shard.ID)
			log.Debugf("Starting consumer for shard %s on %s", shard.ID, shard.AssignedTo)
			go kc.getRecords(shard.ID)
			kc.Add(1)
		}

		select {
		case sig := <-*kc.sigs:
			log.Infof("Received signal %s. Exiting", sig)
			kc.Shutdown()
			return
		case <-*kc.stop:
			log.Info("Shutting down")
			return
		case <-time.After(time.Duration(kc.eventLoopSleepMs) * time.Millisecond):
		}
	}
}

// Shutdown stops consuming records gracefully
func (kc *KinesisConsumer) Shutdown() {
	close(*kc.stop)
	kc.Wait()
}

func (kc *KinesisConsumer) getShardIDs(startShardID string) error {
	args := &kinesis.DescribeStreamInput{
		StreamName: aws.String(kc.StreamName),
	}
	if startShardID != "" {
		args.ExclusiveStartShardId = aws.String(startShardID)
	}

	streamDesc, err := kc.svc.DescribeStream(args)
	if err != nil {
		return err
	}

	if *streamDesc.StreamDescription.StreamStatus != "ACTIVE" {
		return errors.New("Stream not active")
	}

	var lastShardID string
	for _, s := range streamDesc.StreamDescription.Shards {
		if _, ok := kc.shardStatus[*s.ShardId]; !ok {
			log.Debugf("Found shard with id %s", *s.ShardId)
			kc.shardStatus[*s.ShardId] = &shardStatus{
				ID:            *s.ShardId,
				ParentShardID: s.ParentShardId,
			}
		}
		lastShardID = *s.ShardId
	}

	if *streamDesc.StreamDescription.HasMoreShards {
		err := kc.getShardIDs(lastShardID)
		if err != nil {
			return err
		}
	}

	return nil
}

func (kc *KinesisConsumer) getShardIterator(shard *shardStatus) (*string, error) {
	err := kc.checkpointer.FetchCheckpoint(shard)
	if err != nil && err != ErrSequenceIDNotFound {
		return nil, err
	}

	if shard.Checkpoint == "" {
		shardIterArgs := &kinesis.GetShardIteratorInput{
			ShardId:           &shard.ID,
			ShardIteratorType: &kc.ShardIteratorType,
			StreamName:        &kc.StreamName,
		}
		iterResp, err := kc.svc.GetShardIterator(shardIterArgs)
		if err != nil {
			return nil, err
		}
		return iterResp.ShardIterator, nil
	}

	shardIterArgs := &kinesis.GetShardIteratorInput{
		ShardId:                &shard.ID,
		ShardIteratorType:      aws.String("AFTER_SEQUENCE_NUMBER"),
		StartingSequenceNumber: &shard.Checkpoint,
		StreamName:             &kc.StreamName,
	}
	iterResp, err := kc.svc.GetShardIterator(shardIterArgs)
	if err != nil {
		return nil, err
	}
	return iterResp.ShardIterator, nil
}

func (kc *KinesisConsumer) getRecords(shardID string) {
	defer kc.Done()

	shard := kc.shardStatus[shardID]
	shardIterator, err := kc.getShardIterator(shard)
	if err != nil {
		log.Fatalf("Unable to get shard iterator for %s: %s", shardID, err)
	}

	var retriedErrors int

	for {
		getRecordsStartTime := time.Now()
		if time.Now().UTC().After(shard.LeaseTimeout.Add(-5 * time.Second)) {
			err = kc.checkpointer.GetLease(shard, kc.consumerID)
			if err != nil {
				if err.Error() == ErrLeaseNotAquired {
					shard.Lock()
					defer shard.Unlock()
					shard.AssignedTo = ""
					kc.mService.leaseLost(shard.ID)
					log.Debugln("Lease lost for shard", shard.ID, kc.consumerID)
					return
				}
				log.Warnln("Error renewing lease", err)
				time.Sleep(time.Duration(1) * time.Second)
				continue
			}
		}

		getRecordsArgs := &kinesis.GetRecordsInput{
			ShardIterator: shardIterator,
		}
		getResp, err := kc.svc.GetRecords(getRecordsArgs)
		if err != nil {
			if awsErr, ok := err.(awserr.Error); ok {
				if awsErr.Code() == kinesis.ErrCodeProvisionedThroughputExceededException || awsErr.Code() == ErrCodeKMSThrottlingException {
					log.Errorf("Error getting records from shard %v: %v", shardID, err)
					retriedErrors++
					time.Sleep(time.Duration(2^retriedErrors*100) * time.Millisecond)
					continue
				}
			}
			log.Fatalf("Error getting records from Kinesis that cannot be retried: %s\nRequest: %s", err, getRecordsArgs)
		}
		retriedErrors = 0

		var records []*Records
		var recordBytes int64
		for _, r := range getResp.Records {
			record := &Records{
				Data:           r.Data,
				PartitionKey:   *r.PartitionKey,
				SequenceNumber: *r.SequenceNumber,
			}
			records = append(records, record)
			recordBytes += int64(len(record.Data))
			log.Debugf("Processing record %s", *r.SequenceNumber)
		}
		processRecordsStartTime := time.Now()
		kc.RecordConsumer.ProcessRecords(records, kc)

		// Convert from nanoseconds to milliseconds
		processedRecordsTiming := time.Since(processRecordsStartTime) / 1000000
		kc.mService.recordProcessRecordsTime(shard.ID, float64(processedRecordsTiming))

		if len(records) == 0 {
			time.Sleep(time.Duration(kc.EmptyRecordBackoffMs) * time.Millisecond)
		} else if !kc.DisableAutomaticCheckpoints {
			kc.Checkpoint(shardID, *getResp.Records[len(getResp.Records)-1].SequenceNumber)
		}

		kc.mService.incrRecordsProcessed(shard.ID, len(records))
		kc.mService.incrBytesProcessed(shard.ID, recordBytes)
		kc.mService.millisBehindLatest(shard.ID, float64(*getResp.MillisBehindLatest))

		// Convert from nanoseconds to milliseconds
		getRecordsTime := time.Since(getRecordsStartTime) / 1000000
		kc.mService.recordGetRecordsTime(shard.ID, float64(getRecordsTime))

		// The shard has been closed, so no new records can be read from it
		if getResp.NextShardIterator == nil {
			log.Debugf("Shard %s closed", shardID)
			shard := kc.shardStatus[shardID]
			shard.Lock()
			shard.readyToBeClosed = true
			shard.Unlock()
			if !kc.DisableAutomaticCheckpoints {
				kc.Checkpoint(shardID, *getResp.Records[len(getResp.Records)-1].SequenceNumber)
			}
			kc.RecordConsumer.Shutdown()
			return
		}
		shardIterator = getResp.NextShardIterator

		select {
		case <-*kc.stop:
			kc.RecordConsumer.Shutdown()
			return
		case <-time.After(1 * time.Nanosecond):
		}
	}
}

// Checkpoint records the sequence number for the given shard ID as being processed
func (kc *KinesisConsumer) Checkpoint(shardID string, sequenceNumber string) error {
	shard := kc.shardStatus[shardID]
	shard.Lock()
	shard.Checkpoint = sequenceNumber
	shard.Unlock()
	// If shard is closed and we've read all records from the shard, mark the shard as closed
	if shard.readyToBeClosed {
		var err error
		shard.Closed, err = kc.shardIsEmpty(shard)
		if err != nil {
			return err
		}
	}
	return kc.checkpointer.CheckpointSequence(shard)
}

func (kc *KinesisConsumer) shardIsEmpty(shard *shardStatus) (empty bool, err error) {
	iterResp, err := kc.svc.GetShardIterator(&kinesis.GetShardIteratorInput{
		ShardId:                &shard.ID,
		ShardIteratorType:      aws.String("AFTER_SEQUENCE_NUMBER"),
		StartingSequenceNumber: &shard.Checkpoint,
		StreamName:             &kc.StreamName,
	})
	if err != nil {
		return
	}
	recordsResp, err := kc.svc.GetRecords(&kinesis.GetRecordsInput{
		ShardIterator: iterResp.ShardIterator,
	})
	if err != nil {
		return
	}
	if len(recordsResp.Records) == 0 {
		empty = true
	}
	return
}
