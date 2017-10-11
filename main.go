// dataone-indexer
//
// This service listens to the AMQP exchange for the CyVerse data store for events that are relevant to the DataONE
// member node service and records them in the DataONE event database.
package main

import (
	"database/sql"
	"strings"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/dataone-indexer/database"
	"github.com/cyverse-de/dataone-indexer/logger"
	"github.com/cyverse-de/dataone-indexer/model"
	"github.com/cyverse-de/dbutil"
	_ "github.com/lib/pq"
	"github.com/spf13/viper"
	"github.com/streadway/amqp"
	"gopkg.in/alecthomas/kingpin.v2"
)

var defaultConfig = `
amqp:
  uri: amqp://guest:guest@rabbit:5672/jobs
  exchange:
    name: de
  routing-key:
    subscription: data-object.*
    read: data-object.open

db:
  uri: postgresql://guest:guest@dedb:5432/de?sslmode=disable

dataone:
  repository-root: /iplant/home/shared/commons_repo/curated
  node-id: foo
`

// Command-line option definitions.
var (
	config = kingpin.Flag("config", "Path to configuration file.").Short('c').Required().File()
)

// DataoneIndexer represents this service.
type DataoneIndexer struct {
	cfg      *viper.Viper
	messages <-chan amqp.Delivery
	db       *sql.DB
	rootDir  string
	recorder database.Recorder
}

// getDbConnection establishes a connection to the DataONE event database.
func getDbConnection(dburi string) (*sql.DB, error) {

	connector, err := dbutil.NewDefaultConnector("1m")
	if err != nil {
		return nil, err
	}

	db, err := connector.Connect("postgres", dburi)
	if err != nil {
		return nil, err
	}

	return db, nil
}

// getAmqpChannel establishes a connection to the AMQP Broker and returns a channel to use for receiving messages.
func getAmqpChannel(cfg *viper.Viper) (<-chan amqp.Delivery, error) {
	uri := cfg.GetString("amqp.uri")
	exchange := cfg.GetString("amqp.exchange.name")
	queueName := "dataone.events"
	routingKey := cfg.GetString("amqp.routing-key.subscription")

	// Establish the AMQP connection.
	conn, err := amqp.Dial(uri)
	if err != nil {
		return nil, err
	}

	// Create the AMQP channel.
	ch, err := conn.Channel()
	if err != nil {
		return nil, err
	}

	// Declare the queue.
	queue, err := ch.QueueDeclare(
		queueName, // queue name
		false,     // queue durable
		false,     // queue auto-delete flag
		false,     // queue exclusive flag
		false,     // queue no-wait flag
		nil,       // arguments
	)
	if err != nil {
		return nil, err
	}

	// Bind the queue to the routing key.
	err = ch.QueueBind(
		queue.Name, // queue name
		routingKey, // routing key
		exchange,   // exchange name
		false,      // no-wait flag
		nil,        // arguments
	)
	if err != nil {
		return nil, err
	}

	// Create and return the consumer channel.
	return ch.Consume(
		queue.Name, // queue name
		"",         // consumer name,
		true,       // auto-ack flag
		false,      // exclusive flag
		false,      // no-local flag
		false,      // no-wait flag
		nil,        // args
	)
}

// getRoutingKeys returns a structure that the recorder uses to determine how to process AMQP messages based on
// routing key.
func getRoutingKeys(cfg *viper.Viper) *database.KeyNames {
	return &database.KeyNames{
		Read: cfg.GetString("amqp.routing-key.read"),
	}
}

// initService initializes the DataONE indexer service.
func initService() *DataoneIndexer {

	// Parse the command-line options.
	kingpin.Parse()

	// Load the configuration file.
	cfg, err := configurate.InitDefaultsR(*config, configurate.JobServicesDefaults)
	if err != nil {
		logger.Log.Fatalf("Unable to load the configuration: %s", err)
	}

	// Establish the database connection.
	db, err := getDbConnection(cfg.GetString("db.uri"))
	if err != nil {
		logger.Log.Fatalf("Unable to establish the database connection: %s", err)
	}

	// Create the AMQP channel.
	messages, err := getAmqpChannel(cfg)
	if err != nil {
		logger.Log.Fatalf("Unable to subscribe to AMQP messages: %s", err)
	}

	return &DataoneIndexer{
		cfg:      cfg,
		messages: messages,
		db:       db,
		rootDir:  cfg.GetString("dataone.repository-root"),
		recorder: database.NewRecorder(db, getRoutingKeys(cfg), cfg.GetString("dataone.node-id")),
	}
}

// processMessages iterates through incoming AMQP messages and records qualifying events.
func (svc *DataoneIndexer) processMessages() {
	for delivery := range svc.messages {
		key := delivery.RoutingKey
		msg, err := model.Decode(delivery.Body)
		if err != nil {
			logger.Log.Errorf("Unable to parse message (%s): %s", delivery.Body, err)
		}
		if strings.Index(msg.Path, svc.rootDir) == 0 {
			if err := svc.recorder.RecordEvent(key, msg); err != nil {
				logger.Log.Errorf("Unable to record message (%s): %s", delivery.Body, err)
			}
		}
	}
}

// main initializes and runs the DataONE indexer service.
func main() {
	svc := initService()

	// Listen for incoming messages forever.
	logger.Log.Info("waiting for incoming AMQP messages")
	spinner := make(chan bool)
	go func() {
		svc.processMessages()
	}()
	<-spinner
}
