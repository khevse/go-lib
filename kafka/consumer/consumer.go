package consumer

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type FuncOnError func(ctx context.Context, logger *zap.Logger, err error)
type FuncOnProcess func(ctx context.Context, logger *zap.Logger, msg *kafka.Message, s ISleeper) error
type FuncOnCommit func(ctx context.Context, logger *zap.Logger, topic string, partition int32, offset kafka.Offset, committed int)
type FuncOnRevoke func(ctx context.Context, logger *zap.Logger, topic []kafka.TopicPartition)
type FuncOnRebalance func(ctx context.Context, logger *zap.Logger, topic []kafka.TopicPartition)

var (
	nopCommitFunc = func(ctx context.Context, logger *zap.Logger, topic string, partition int32, offset kafka.Offset, committed int) {
	}
	nopOnRevokeFunc    = func(ctx context.Context, logger *zap.Logger, topic []kafka.TopicPartition) {}
	nopOnRebalanceFunc = func(ctx context.Context, logger *zap.Logger, topic []kafka.TopicPartition) {}
)

type Consumer struct {
	observable

	id                   uuid.UUID
	commitOffsetCount    int
	commitOffsetDuration time.Duration
	ctx                  context.Context
	ctxCancel            context.CancelFunc
	logger               *zap.Logger
	onCommit             FuncOnCommit
	onError              FuncOnError
	onProcess            FuncOnProcess
	onRevoke             FuncOnRevoke
	onRebalance          FuncOnRebalance
	reader               *kafka.Consumer
	topics               []string
	wg                   sync.WaitGroup
	mu                   sync.RWMutex
}

func New(cfg *Config, logger *zap.Logger) (*Consumer, error) {

	if err := cfg.Check(); err != nil {
		return nil, err
	}

	onCommit := nopCommitFunc
	if cfg.OnCommit != nil {
		onCommit = cfg.OnCommit
	}

	onRevoke := nopOnRevokeFunc
	if cfg.OnRevoke != nil {
		onRevoke = cfg.OnRevoke
	}

	onRebalance := nopOnRebalanceFunc
	if cfg.OnRebalance != nil {
		onRebalance = cfg.OnRebalance
	}

	id, err := uuid.NewUUID()
	if err != nil {
		return nil, err
	}

	requiredProps := kafka.ConfigMap{
		// https://godoc.org/github.com/confluentinc/confluent-kafka-go/kafka#hdr-Consumer_events
		"enable.auto.commit": false,
		// https://godoc.org/github.com/confluentinc/confluent-kafka-go/kafka#NewConsumer
		"go.events.channel.enable": true,
		"go.events.channel.size":   0, // don't use channel buffer
		// https://godoc.org/github.com/confluentinc/confluent-kafka-go/kafka#NewConsumer
		"go.application.rebalance.enable": true,
		// https://docs.confluent.io/3.3.1/clients/librdkafka/CONFIGURATION_8md.html
		"api.version.request": "true",
		"client.id":           id.String(),
	}
	for k, v := range requiredProps {
		if err := cfg.ConfigMap.SetKey(k, v); err != nil {
			return nil, errors.Wrapf(err, "force set config %s to %v failed", k, v)
		}
	}

	logger = logger.With(zap.String("consumer", id.String()))

	ctx, ctxCancel := context.WithCancel(context.Background())

	reader, err := kafka.NewConsumer(cfg.ConfigMap)
	if err != nil {
		defer ctxCancel()
		return nil, errors.Wrap(err, "create reader failed")
	}

	return &Consumer{
		id:                   id,
		ctx:                  ctx,
		ctxCancel:            ctxCancel,
		logger:               logger,
		onCommit:             onCommit,
		onRevoke:             onRevoke,
		onRebalance:          onRebalance,
		onError:              cfg.OnError,
		onProcess:            cfg.OnProcess,
		reader:               reader,
		topics:               cfg.Topics,
		commitOffsetCount:    cfg.CommitOffsetCount,
		commitOffsetDuration: cfg.CommitOffsetDuration,
		observable:           *newObservable(),
	}, nil
}

func (c *Consumer) Start() error {

	defer func() { c.observable.notify(StateClosed) }()

	c.mu.Lock()
	defer func() {
		c.ctxCancel()
		c.mu.Unlock()
	}()

	c.wg.Add(1)
	defer c.wg.Done()

	c.logger.Info("start")
	defer func() {
		if err := recover(); err != nil {
			c.logger.Error("panic:" + fmt.Sprintln(err))
		}
	}()

	select {
	case <-c.ctx.Done():
		// protection for error of double closing: 'fatal error: unexpected signal during runtime execution'
		err := errors.New("consumer already closed")
		c.logger.Error("failed to start consumer", zap.Error(err))
		return err
	default:
		// ok
	}

	c.observable.notify(StateRun)

	return c.listen()
}

func (c *Consumer) Stop() {

	c.ctxCancel()

	c.mu.Lock() // protection for WaitGroup data race
	defer c.mu.Unlock()

	c.wg.Wait()
}

func (c *Consumer) Sleep(delay time.Duration, partitions []kafka.TopicPartition) error {
	if len(partitions) == 0 {
		return nil
	}
	err := c.reader.Pause(partitions)
	if err != nil {
		c.logger.With(
			zap.Any("partitions", partitions),
		).Warn("failed to pause consumer", zap.Error(err))
		return err
	}

	go func() {
		time.Sleep(delay)
		select {
		case <-c.ctx.Done():
			c.logger.Warn("service already stopped")
		default:
			err := c.reader.Resume(partitions)
			if err != nil {
				c.logger.With(
					zap.Any("partitions", partitions),
				).Warn("failed to resume consumer", zap.Error(err))
			}

			//Resume doesn't return error if broker is unavailable, that's why we try to get metadata
			for _, partition := range partitions {
				_, err = c.reader.GetMetadata(partition.Topic, false, 2000)
				if err != nil {
					c.logger.With(
						zap.String("topic", *partition.Topic),
						zap.Int32("partition", partition.Partition),
					).Warn("may be partition haven't been resumed", zap.Error(err))
				}
			}
		}
	}()

	return nil
}

func (c *Consumer) listen() error {
	c.logger.Info("start listener")
	err := c.reader.SubscribeTopics(c.topics, nil)
	if err != nil {
		return errors.Wrap(err, "subscribe to topics failed")
	}

	defer func() {
		defer c.logger.Info("done")

		c.logger.Info("closing...")
		// logs for issues:
		// https://github.com/confluentinc/confluent-kafka-go/issues/65
		// https://github.com/confluentinc/confluent-kafka-go/issues/189
		defer c.logger.Info("success closed")

		if errUnsubscribe := c.reader.Unsubscribe(); errUnsubscribe != nil {
			c.logger.Error("failed to unsubscribe", zap.Error(errUnsubscribe))
		} else if errUnassign := c.reader.Unassign(); errUnassign != nil {
			c.logger.Error("failed to unassign", zap.Error(errUnassign))
		}

		if err := c.reader.Close(); err != nil {
			c.logger.Error("failed to close reader", zap.Error(err))
		}
	}()

	consumerOffsets := newOffset()
	defer c.commitOffsets(consumerOffsets)

	commitOffsetDuration := c.commitOffsetDuration
	if commitOffsetDuration <= 0 {
		commitOffsetDuration = time.Second * 5
	}

	offsetsTicker := time.NewTicker(commitOffsetDuration)
	defer offsetsTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return nil

		case <-offsetsTicker.C:
			c.commitOffsets(consumerOffsets)

		case ev := <-c.reader.Events():

			switch e := ev.(type) {
			case kafka.AssignedPartitions:
				// consumer group rebalance event: assigned partition set
				if err := c.handleRebalance(&e, consumerOffsets); err != nil {
					return err
				}

			case kafka.RevokedPartitions:
				// consumer group rebalance event: revoked partition set
				if err := c.handleRevoke(&e, consumerOffsets); err != nil {
					return err
				}

			case *kafka.Message:
				if err := c.handleMessage(e, consumerOffsets); err != nil {
					return err
				}

			case kafka.PartitionEOF:
				// consumer reached end of partition
				// Needs to be explicitly enabled by setting the `enable.partition.eof`
				// configuration property to true.
				if err := c.handlePartitionEOF(&e, consumerOffsets); err != nil {
					return err
				}

			case kafka.OffsetsCommitted:
				// reports committed offsets
				// https://godoc.org/github.com/confluentinc/confluent-kafka-go/kafka#hdr-Consumer_events
				// Offset commit results (when `enable.auto.commit` is enabled)
				if err := c.handleOffsetCommitted(&e, consumerOffsets); err != nil {
					return err
				}

			case kafka.Error:
				// Errors should generally be considered as informational, the client will try to automatically recover
				c.logger.Error("error event", zap.Error(e))
				c.onError(c.ctx, c.logger, e)

			default:
				c.logger.Error("unknown event", zap.Any("payload", e))
			}

		}
	}
}

func (c *Consumer) handleRebalance(e *kafka.AssignedPartitions, consumerOffsets *offset) error {

	c.commitOffsets(consumerOffsets)
	consumerOffsets.Clear()

	opLog := c.logger.With(zap.String("operation", "rebalance"), zap.Any("event", e))

	if err := checkPartitions(e.Partitions); err != nil {
		opLog.Error("failed to check assigned partitions", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	// Issue: https://github.com/confluentinc/confluent-kafka-go/issues/212
	// All offsets in topics are equal -1001(unset).
	// If assign the event with these invalid value, duplicate messages can appear.
	// Fix: read committed offsets before assign new topics.
	committedOffsets, err := c.reader.Committed(e.Partitions, 5000)
	if err != nil {
		opLog.Error("failed to read committed offsets", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	for i := range committedOffsets {
		item := &committedOffsets[i]
		if item.Offset >= 0 {
			item.Offset++
		}
	}

	if err := c.reader.Assign(committedOffsets); err != nil {
		opLog.Error("failed to set assigned", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	c.onRebalance(c.ctx, opLog, e.Partitions)
	opLog.Info("success")

	return nil
}

func (c *Consumer) handleRevoke(e *kafka.RevokedPartitions, consumerOffsets *offset) error {

	c.commitOffsets(consumerOffsets)
	consumerOffsets.Clear()

	opLog := c.logger.With(zap.String("operation", "revoked"), zap.Any("event", e))

	if err := checkPartitions(e.Partitions); err != nil {
		opLog.Error("failed to check revoked partitions", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	if err := c.reader.Unassign(); err != nil {
		opLog.Error("failed to unassign", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	c.onRevoke(c.ctx, opLog, e.Partitions)
	opLog.Info("success")

	return nil
}

func (c *Consumer) handleMessage(e *kafka.Message, consumerOffsets *offset) error {

	opLog := c.logger.With(zap.String("operation", "message"), zap.Any("event", e))

	if err := e.TopicPartition.Error; err != nil {
		opLog.Error("failed to check", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	if err := c.onProcess(c.ctx, opLog, e, c); err != nil {
		opLog.Error("failed to process message", zap.Error(err))
		return err
	}

	consumerOffsets.Add(e.TopicPartition)

	if c.commitOffsetCount > 0 {
		if consumerOffsets.Counter() >= c.commitOffsetCount {
			c.commitOffsets(consumerOffsets)
		}
	}

	opLog.Debug("success")
	return nil
}

func (c *Consumer) handlePartitionEOF(e *kafka.PartitionEOF, consumerOffsets *offset) error {

	c.commitOffsets(consumerOffsets)
	consumerOffsets.Clear()

	opLog := c.logger.With(zap.String("operation", "partition EOF"), zap.Any("event", e))

	if err := e.Error; err != nil {
		if kafkaErr, ok := err.(kafka.Error); !ok || kafkaErr.Code() != kafka.ErrPartitionEOF {
			opLog.Error("failed to check", zap.Error(err))
			c.onError(c.ctx, opLog, err)
			return err
		}
	}

	opLog.Info("success")
	return nil
}

func (c *Consumer) handleOffsetCommitted(e *kafka.OffsetsCommitted, consumerOffsets *offset) error {

	opLog := c.logger.With(zap.String("operation", "committed offsets"), zap.Any("event", e))

	if err := e.Error; err != nil {
		opLog.Error("committed offsets", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	if err := checkPartitions(e.Offsets); err != nil {
		opLog.Error("failed to check", zap.Error(err))
		c.onError(c.ctx, opLog, err)
		return err
	}

	opLog.Info("success")
	return nil
}

func (c *Consumer) commitOffsets(consumerOffsets *offset) {

	list, count := consumerOffsets.Get()
	if len(list) > 0 {
		opLog := c.logger.WithOptions(zap.AddCallerSkip(1)).With(
			zap.String("operation", "commit offsets"),
			zap.Any("event", list))

		success, err := c.reader.CommitOffsets(list)
		if err != nil {
			opLog.Error("failed to commit", zap.Error(err))
			c.onError(c.ctx, opLog, err)
			return
		}

		if err := checkPartitions(success); err != nil {
			opLog.Error("failed to commit", zap.Error(err))
			c.onError(c.ctx, opLog, err)
			return
		}

		opLog.Debug("success", zap.Any("result", success))

		var topic string
		for i := range success {
			item := &success[i]
			consumerOffsets.Remove(*item)

			if item.Topic != nil {
				topic = *item.Topic
			}

			countCommitted := count[getPartitionKey(item.Topic, item.Partition)]
			c.onCommit(c.ctx, opLog, topic, item.Partition, item.Offset, countCommitted)
		}
	}
}
