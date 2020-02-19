package kafka

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudfoundry/sonde-go/events"
	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/evoila/cf-kafka-nozzle/autoscaler"
	"github.com/evoila/cf-kafka-nozzle/config"
	"github.com/evoila/cf-kafka-nozzle/redisClient"
	"github.com/evoila/cf-kafka-nozzle/stats"
	"golang.org/x/net/context"
)

const (
	// TopicAppLogTmpl is Kafka topic name template for LogMessage
	TopicAppLogTmpl = "app-log-%s"

	// TopicCFMetrics is Kafka topic name for ValueMetric
	TopicCFMetric = "cf-metrics"
)

const (
	// Default topic name for each event
	DefaultValueMetricTopic = "value-metric"
	DefaultLogMessageTopic  = "log-message"

	DefaultKafkaRepartitionMax = 5
	DefaultKafkaRetryMax       = 1
	DefaultKafkaRetryBackoff   = 100 * time.Millisecond

	DefaultChannelBufferSize  = 512
	DefaultSubInputBufferSize = 1024
)

//Create new KafkaProducer with the possibility to handle security
func NewKafkaProducer(logger *log.Logger, stats *stats.Stats, config *config.Config) (NozzleProducer, error) {
	brokers := config.Kafka.Brokers
	if len(brokers) < 1 {
		return nil, fmt.Errorf("brokers are not provided")
	}

	producerConfig := kafka.ConfigMap{
		"bootstrap.servers":             strings.Join(brokers, ", "),
		"partition.assignment.strategy": "roundrobin",
		"retries":                       DefaultKafkaRetryMax,
		"retry.backoff.ms":              DefaultKafkaRetryBackoff,
	}

	if config.Kafka.Secure {
		producerConfig.SetKey("security.protocol", "sasl_ssl")
		producerConfig.SetKey("sasl.mechanism", "SCRAM-SHA-256")
		producerConfig.SetKey("sasl.username", config.Kafka.SaslUsername)
		producerConfig.SetKey("sasl.password", config.Kafka.SaslPassword)
		producerConfig.SetKey("ssl.ca.location", os.TempDir()+config.Kafka.Filename)
	}

	if config.Kafka.RetryMax != 0 {
		producerConfig.SetKey("retries", config.Kafka.RetryMax)
	}

	if config.Kafka.RetryBackoff != 0 {
		backoff := time.Duration(config.Kafka.RetryBackoff) * time.Millisecond
		producerConfig.SetKey("retry.backoff.ms", backoff)
	}

	producer, err := kafka.NewProducer(&producerConfig)
	if err != nil {
		return nil, err
	}

	repartitionMax := DefaultKafkaRepartitionMax
	if config.Kafka.RepartitionMax != 0 {
		repartitionMax = config.Kafka.RepartitionMax
	}

	return &KafkaProducer{
		Producer:                       producer,
		Logger:                         logger,
		Stats:                          stats,
		logMessageTopic:                config.Kafka.Topic.LogMessage,
		autoscalerContainerMetricTopic: config.Kafka.Topic.AutoscalerContainerMetric,
		logMetricContainerMetricTopic:  config.Kafka.Topic.LogMetricContainerMetric,
		httpMetricTopic:                config.Kafka.Topic.HttpMetric,
		repartitionMax:                 repartitionMax,
		errors:                         make(chan *kafka.Error),
		deliveryChan:                   make(chan kafka.Event),
	}, nil
}

// KafkaProducer implements NozzleProducer interfaces
type KafkaProducer struct {
	*kafka.Producer

	repartitionMax int
	errors         chan *kafka.Error

	logMessageTopic                string
	autoscalerContainerMetricTopic string
	logMetricContainerMetricTopic  string
	httpMetricTopic                string

	Logger *log.Logger
	Stats  *stats.Stats

	deliveryChan chan kafka.Event

	once sync.Once
}

// metadata is metadata which will be injected to ProducerMessage.Metadata.
// This is used only when publish is failed and re-partitioning by ourself.
type metadata struct {
	// retires is the number of re-partitioning
	retries int
}

// init sets default logger
func (kp *KafkaProducer) init() {
	if kp.Logger == nil {
		kp.Logger = defaultLogger
	}
}

func (kp *KafkaProducer) Errors() <-chan *kafka.Error {
	return kp.errors
}

func (kp *KafkaProducer) Successes() <-chan *kafka.Message {
	msgCh := make(chan *kafka.Message)
	return msgCh
}

// Produce produces event to kafka
func (kp *KafkaProducer) Produce(ctx context.Context, eventCh <-chan *events.Envelope) {
	kp.once.Do(kp.init)

	kp.Logger.Printf("[INFO] Start to sub input")

	kp.Logger.Printf("[INFO] Start loop to watch events")

	for {
		select {
		case <-ctx.Done():
			// Stop process immediately
			kp.Logger.Printf("[INFO] Stop kafka producer")
			return

		case event, ok := <-eventCh:
			if !ok {
				kp.Logger.Printf("[ERROR] Nozzle consumer eventCh is closed")
				return
			}

			kp.input(event)
		}
	}
}

func (kp *KafkaProducer) input(event *events.Envelope) {
	switch event.GetEventType() {
	case events.Envelope_HttpStart:
		// Do nothing
	case events.Envelope_HttpStartStop:
		var appId string = uuidToString(event.GetHttpStartStop().GetApplicationId())
		if appId != "" {
			redisEntry := getAppData(appId)
			if event.GetHttpStartStop().GetPeerType() == 1 &&
				checkIfPublishIsPossible(appId, redisEntry) {

				latency := event.GetHttpStartStop().GetStopTimestamp() - event.GetHttpStartStop().GetStartTimestamp()

				httpMetric := &autoscaler.HttpMetric{
					Timestamp:        event.GetTimestamp() / 1000 / 1000, //convert to ms
					MetricName:       "HttpMetric",
					AppId:            appId,
					AppName:          redisEntry["appName"].(string),
					Space:            redisEntry["space"].(string),
					SpaceId: 		  redisEntry["spaceId"].(string),
					Organization:     redisEntry["organization"].(string),
					OrganizationGuid: redisEntry["organizationGuid"].(string),
					Requests:         1,
					Latency:          int32(latency) / 1000 / 1000, //convert to ms
					Description:      "Statuscode: " + strconv.Itoa(int(event.GetHttpStartStop().GetStatusCode())),
				}

				jsonHttpMetric, err := json.Marshal(httpMetric)

				if err == nil {
					err = kp.Producer.Produce(&kafka.Message{
						TopicPartition: kafka.TopicPartition{Topic: &kp.httpMetricTopic, Partition: kafka.PartitionAny},
						Value:          jsonHttpMetric,
					}, kp.deliveryChan)

					if err != nil {
						fmt.Printf("[ERROR] Could not enqueue message on kafka: " + err.Error())
						kp.Stats.Inc(stats.PublishFail)
					}

					kp.Stats.Inc(stats.Consume)
				}
			}
		}
	case events.Envelope_HttpStop:
		// Do nothing
	case events.Envelope_LogMessage:
		var appId string = event.GetLogMessage().GetAppId()
		if appId != "" {
			redisEntry := getAppData(appId)
			if checkIfPublishIsPossible(appId, redisEntry) && checkIfSourceTypeIsValid(event.GetLogMessage().GetSourceType()) {
				logMessage := &autoscaler.LogMessage{
					Timestamp:        event.GetLogMessage().GetTimestamp() / 1000 / 1000,
					LogMessage:       string(event.GetLogMessage().GetMessage()[:]),
					LogMessageType:   event.GetLogMessage().GetMessageType().String(),
					SourceType:       event.GetLogMessage().GetSourceType(),
					AppId:            event.GetLogMessage().GetAppId(),
					AppName:          redisEntry["appName"].(string),
					Space:            redisEntry["space"].(string),
					SpaceId: 		  redisEntry["spaceId"].(string),
					Organization:     redisEntry["organization"].(string),
					OrganizationGuid: redisEntry["organizationGuid"].(string),
					SourceInstance:   event.GetLogMessage().GetSourceInstance(),
				}

				jsonLogMessage, err := json.Marshal(logMessage)

				if err == nil {
					err = kp.Producer.Produce(&kafka.Message{
						TopicPartition: kafka.TopicPartition{Topic: &kp.logMessageTopic, Partition: kafka.PartitionAny},
						Value:          jsonLogMessage,
					}, kp.deliveryChan)

					if err != nil {
						fmt.Printf("[ERROR] Could not enqueue message on kafka: " + err.Error())
						kp.Stats.Inc(stats.PublishFail)
					}

					kp.Stats.Inc(stats.Consume)
				}
			}
		}

	case events.Envelope_ValueMetric:
		/*kp.Input() <- &sarama.ProducerMessage{
			Topic:    kp.ValueMetricTopic(),
			Value:    &JsonEncoder{event: event},
			Metadata: metadata{retries: 0},
		}*/
	case events.Envelope_CounterEvent:
		// Do nothing
	case events.Envelope_Error:
		// Do nothing
	case events.Envelope_ContainerMetric:
		var appId string = event.GetContainerMetric().GetApplicationId()
		if appId != "" {

			redisEntry := getAppData(appId)
			if checkIfPublishIsPossible(appId, redisEntry) {

				containerMetric := &autoscaler.ContainerMetric{
					Timestamp:        event.GetTimestamp() / 1000 / 1000, //convert to ms
					MetricName:       "InstanceContainerMetric",
					AppId:            appId,
					AppName:          redisEntry["appName"].(string),
					Space:            redisEntry["space"].(string),
					SpaceId: 		  redisEntry["spaceId"].(string),
					Organization:     redisEntry["organization"].(string),
					OrganizationGuid: redisEntry["organizationGuid"].(string),
					Cpu:              int32(event.GetContainerMetric().GetCpuPercentage()), //* 100),
					Ram:              int64(event.GetContainerMetric().GetMemoryBytes()),
					InstanceIndex:    event.GetContainerMetric().GetInstanceIndex(),
					Description:      "",
				}

				jsonContainerMetric, err := json.Marshal(containerMetric)

				if err == nil {
					err = kp.Producer.Produce(&kafka.Message{
						TopicPartition: kafka.TopicPartition{Topic: &kp.logMetricContainerMetricTopic, Partition: kafka.PartitionAny},
						Value:          jsonContainerMetric,
					}, kp.deliveryChan)

					if err != nil {
						fmt.Printf("[ERROR] Could not enqueue message on kafka: " + err.Error())
						kp.Stats.Inc(stats.PublishFail)
					}

					kp.Stats.Inc(stats.Consume)
				}
			}
		}
	}
}

func (kp *KafkaProducer) ReadDeliveryChan() {

	// Only checks if report is received since there are no reports for
	// failed deliveries
	for {
		<-kp.deliveryChan
		kp.Stats.Inc(stats.Publish)
	}
}

func getAppData(appId string) map[string]interface{} {
	var data map[string]interface{}
	json.Unmarshal([]byte(redisClient.Get(appId)), &data)
	return data
}

// This function does NOT creates a request to redis when appId is not empty, but works with the given map
// Use the getAppData(appId) function to get the corresponding map
func checkIfPublishIsPossible(appId string, redisEntry map[string]interface{}) bool {
	if len(redisEntry) == 0 {
		redisClient.Set(appId, "{\"subscribed\":false}", 0)
		return false
	}
	return redisEntry["subscribed"].(bool)
}

func checkIfSourceTypeIsValid(sourceType string) bool {
	return sourceType == "RTR" || sourceType == "STG" || sourceType == "APP/PROC/WEB"
}

func uuidToString(uuid *events.UUID) string {
	var lowerarray = make([]byte, 8)
	var higherarray = make([]byte, 8)
	binary.LittleEndian.PutUint64(lowerarray, uuid.GetLow())
	binary.LittleEndian.PutUint64(higherarray, uuid.GetHigh())

	var bytearray = addBytes(lowerarray, higherarray)
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		bytearray[:4], bytearray[4:6], bytearray[6:8], bytearray[8:10], bytearray[10:])
}

func addBytes(arrayLow []byte, arrayHigh []byte) []byte {
	var bytearray = make([]byte, 16)
	for i := 0; i <= 7; i++ {
		//j := i + 8
		bytearray[0+i] = arrayLow[i]
		bytearray[8+i] = arrayHigh[i]

	}
	return bytearray
}
