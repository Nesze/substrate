package kafka

import (
	"context"
	"time"

	"github.com/Shopify/sarama"
	"github.com/bsm/sarama-cluster"
	"github.com/uw-labs/substrate"
)

var (
	_ substrate.AsyncMessageSink   = (*AsyncMessageSink)(nil)
	_ substrate.AsyncMessageSource = (*AsyncMessageSource)(nil)
)

const (
	// OffsetOldest indicates the oldest appropriate message available on the broker.
	OffsetOldest int64 = -2
	// OffsetNewest indicates the next appropriate message available on the broker.
	OffsetNewest int64 = -1

	defaultMetadataRefreshFrequency = 10 * time.Minute
)

type AsyncMessageSinkConfig struct {
	Brokers         []string
	Topic           string
	MaxMessageBytes int
	KeyFunc         func(substrate.Message) []byte
	Version         *sarama.KafkaVersion
}

func NewAsyncMessageSink(config AsyncMessageSinkConfig) (substrate.AsyncMessageSink, error) {

	conf := config.buildSaramaProducerConfig()
	client, err := sarama.NewClient(config.Brokers, conf)
	if err != nil {
		return nil, err
	}

	sink := AsyncMessageSink{
		client:  client,
		Topic:   config.Topic,
		KeyFunc: config.KeyFunc,
	}
	return &sink, nil
}

type AsyncMessageSink struct {
	client  sarama.Client
	Topic   string
	KeyFunc func(substrate.Message) []byte
}

// PublishMessages implements the PublishMessages method of the
// substrate.AsyncMessageSink interface.
func (ams *AsyncMessageSink) PublishMessages(ctx context.Context, acks chan<- substrate.Message, messages <-chan substrate.Message) (rerr error) {

	producer, err := sarama.NewAsyncProducerFromClient(ams.client)
	if err != nil {
		return err
	}

	err = ams.doPublishMessages(ctx, producer, acks, messages)
	if err != nil {
		_ = producer.Close()
		return err
	}
	return producer.Close()
}

func (ams *AsyncMessageSink) doPublishMessages(ctx context.Context, producer sarama.AsyncProducer, acks chan<- substrate.Message, messages <-chan substrate.Message) (rerr error) {

	input := producer.Input()
	errs := producer.Errors()
	successes := producer.Successes()

	go func() {
		for suc := range successes {
			acks <- suc.Metadata.(substrate.Message)
		}
	}()
	for {
		select {
		case m := <-messages:
			message := &sarama.ProducerMessage{
				Topic: ams.Topic,
			}

			message.Value = sarama.ByteEncoder(m.Data())

			if ams.KeyFunc != nil {
				message.Key = sarama.ByteEncoder(ams.KeyFunc(m))
			}

			message.Metadata = m
			input <- message
		case <-ctx.Done():
			return nil
		case err := <-errs:
			return err
		}
	}
}

func (ams *AsyncMessageSink) Status() (*substrate.Status, error) {
	return status(ams.client, ams.Topic)
}

func (ams *AsyncMessageSinkConfig) buildSaramaProducerConfig() *sarama.Config {
	conf := sarama.NewConfig()
	conf.Producer.RequiredAcks = sarama.WaitForAll // make configurable
	conf.Producer.Return.Successes = true
	conf.Producer.Return.Errors = true
	conf.Producer.Retry.Max = 3
	conf.Producer.Timeout = time.Duration(60) * time.Second

	if ams.MaxMessageBytes != 0 {
		if ams.MaxMessageBytes > int(sarama.MaxRequestSize) {
			sarama.MaxRequestSize = int32(ams.MaxMessageBytes)
		}
		conf.Producer.MaxMessageBytes = int(ams.MaxMessageBytes)
	}

	if ams.KeyFunc != nil {
		conf.Producer.Partitioner = sarama.NewHashPartitioner
	} else {
		conf.Producer.Partitioner = sarama.NewRoundRobinPartitioner
	}

	if ams.Version != nil {
		conf.Version = *ams.Version
	}
	return conf
}

// Close implements the Close method of the substrate.AsyncMessageSink
// interface.
func (ams *AsyncMessageSink) Close() error {
	return ams.client.Close()
}

// AsyncMessageSource represents a kafka message source and implements the
// substrate.AsyncMessageSource interface.
type AsyncMessageSourceConfig struct {
	ConsumerGroup            string
	Topic                    string
	Brokers                  []string
	Offset                   int64
	MetadataRefreshFrequency time.Duration
	OffsetsRetention         time.Duration
	Version                  *sarama.KafkaVersion
}

func (ams *AsyncMessageSourceConfig) buildSaramaConsumerConfig() *cluster.Config {
	offset := OffsetNewest
	if ams.Offset != 0 {
		offset = ams.Offset
	}
	mrf := defaultMetadataRefreshFrequency
	if ams.MetadataRefreshFrequency > 0 {
		mrf = ams.MetadataRefreshFrequency
	}

	config := cluster.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Offsets.Initial = offset
	config.Metadata.RefreshFrequency = mrf
	config.Consumer.Offsets.Retention = ams.OffsetsRetention

	if ams.Version != nil {
		config.Version = *ams.Version
	}
	return config
}

func NewAsyncMessageSource(c AsyncMessageSourceConfig) (substrate.AsyncMessageSource, error) {
	config := c.buildSaramaConsumerConfig()

	client, err := cluster.NewClient(c.Brokers, config)
	if err != nil {
		return nil, err
	}

	return &AsyncMessageSource{
		client:        client,
		consumerGroup: c.ConsumerGroup,
		topic:         c.Topic,
	}, nil
}

type AsyncMessageSource struct {
	client        *cluster.Client
	consumerGroup string
	topic         string
}

type consumerMessage struct {
	cm *sarama.ConsumerMessage
}

func (cm *consumerMessage) Data() []byte {
	return cm.cm.Value
}

// ConsumeMessages implements the ConsumeMessages method of the substrate.AsyncMessageSource interface.
func (ams *AsyncMessageSource) ConsumeMessages(ctx context.Context, messages chan<- substrate.Message, acks <-chan substrate.Message) error {

	c, err := cluster.NewConsumerFromClient(ams.client, ams.consumerGroup, []string{ams.topic})
	if err != nil {
		return err
	}

	defer func() {
		_ = c.Close()
	}()

	var forAcking []*consumerMessage

	for {
		select {
		case msg := <-c.Messages():

			message := &consumerMessage{
				cm: msg,
			}

			select {
			case <-ctx.Done():
				return c.Close()
			case messages <- message:
			}

			forAcking = append(forAcking, message)
		case ack := <-acks:
			switch {
			case len(forAcking) == 0:
				return substrate.InvalidAckError{
					Acked:    ack,
					Expected: nil,
				}
			case ack != forAcking[0]:
				return substrate.InvalidAckError{
					Acked:    ack,
					Expected: forAcking[0],
				}
			default:
				c.MarkOffset(forAcking[0].cm, "")
				forAcking = forAcking[1:]
			}

		case err := <-c.Errors():
			return err
		case <-ctx.Done():
			return c.Close()
		}
	}
}

func (ams *AsyncMessageSource) Status() (*substrate.Status, error) {
	return status(ams.client, ams.topic)
}

// Close implements the Close method of the substrate.AsyncMessageSource
// interface.
func (ams *AsyncMessageSource) Close() error {
	return ams.client.Close()

}