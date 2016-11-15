package imap

import (
	"fmt"
	"log"
	"net"
	"sync"

	"crypto/tls"

	"github.com/numbleroot/pluto/config"
	"github.com/numbleroot/pluto/crypto"
)

// Worker struct bundles information needed in
// operation of a worker node.
type Worker struct {
	lock        sync.RWMutex
	Name        string
	MailSocket  net.Listener
	SyncSocket  net.Listener
	Connections map[string]*tls.Conn
	Contexts    map[string]*Context
	Config      *config.Config
}

// Functions

// InitWorker listens for TLS connections on a TCP socket
// opened up on supplied IP address and port as well as connects
// to involved storage node. It returns those information bundeled
// in above Worker struct.
func InitWorker(config *config.Config, workerName string) (*Worker, error) {

	var err error

	// Initialize and set fields.
	worker := &Worker{
		lock:        sync.RWMutex{},
		Name:        workerName,
		Connections: make(map[string]*tls.Conn),
		Contexts:    make(map[string]*Context),
		Config:      config,
	}

	// Check if supplied worker with workerName actually is configured.
	if _, ok := config.Workers[worker.Name]; !ok {

		var workerID string

		// Retrieve first valid worker ID to provide feedback.
		for workerID = range config.Workers {
			break
		}

		return nil, fmt.Errorf("[imap.InitWorker] Specified worker ID does not exist in config file. Please provide a valid one, for example '%s'.\n", workerID)
	}

	// Load internal TLS config.
	internalTLSConfig, err := crypto.NewInternalTLSConfig(config.Workers[worker.Name].TLS.CertLoc, config.Workers[worker.Name].TLS.KeyLoc, config.RootCertLoc)
	if err != nil {
		return nil, err
	}

	// Try to connect to sync port of storage node with internal TLS config.
	c, err := tls.Dial("tcp", fmt.Sprintf("%s:%s", config.Storage.IP, config.Storage.SyncPort), internalTLSConfig)
	if err != nil {
		return nil, fmt.Errorf("[imap.InitWorker] Could not connect to sync port of storage node because of: %s\n", err.Error())
	}

	// Save connection for later use.
	worker.Connections["storage"] = c

	// Start to listen for incoming internal connections on defined IP and sync port.
	worker.SyncSocket, err = tls.Listen("tcp", fmt.Sprintf("%s:%s", config.Workers[worker.Name].IP, config.Workers[worker.Name].SyncPort), internalTLSConfig)
	if err != nil {
		return nil, fmt.Errorf("[imap.InitWorker] Listening for internal sync TLS connections failed with: %s\n", err.Error())
	}

	// TODO: Dispatch into receiving goroutine on sync connection.

	// Start to listen for incoming internal connections on defined IP and mail port.
	worker.MailSocket, err = tls.Listen("tcp", fmt.Sprintf("%s:%s", config.Workers[worker.Name].IP, config.Workers[worker.Name].MailPort), internalTLSConfig)
	if err != nil {
		return nil, fmt.Errorf("[imap.InitWorker] Listening for internal IMAP TLS connections failed with: %s\n", err.Error())
	}

	log.Printf("[imap.InitWorker] Listening for incoming IMAP requests on %s.\n", worker.MailSocket.Addr())

	return worker, nil
}

// Run loops over incoming requests at worker and
// dispatches each one to a goroutine taking care of
// the commands supplied.
func (worker *Worker) Run() error {

	for {

		// Accept request or fail on error.
		conn, err := worker.MailSocket.Accept()
		if err != nil {
			return fmt.Errorf("[imap.Run] Accepting incoming request at %s failed with: %s\n", worker.Name, err.Error())
		}

		// Dispatch into own goroutine.
		go worker.HandleConnection(conn)
	}
}

// HandleConnection is the main worker routine where all
// incoming requests against worker nodes have to go through.
func (worker *Worker) HandleConnection(conn net.Conn) {

	// Create a new connection struct for incoming request.
	c := NewConnection(conn)

	// Receive opening information.
	opening, err := c.Receive()
	if err != nil {
		c.ErrorLogOnly("Encountered receive error", err)
		return
	}

	// As long as the distributor node did not indicate that
	// the system is about to shut down, we accept requests.
	for opening != "> done <" {

		// Extract the prefixed clientID and update or create context.
		clientID, err := worker.UpdateClientContext(opening)
		if err != nil {
			c.ErrorLogOnly("Error extracting context", err)
			return
		}

		// Receive incoming actual client command.
		rawReq, err := c.Receive()
		if err != nil {
			c.ErrorLogOnly("Encountered receive error", err)
			return
		}

		// Parse received next raw request into struct.
		req, err := ParseRequest(rawReq)
		if err != nil {

			// Signal error to client.
			err := c.Send(err.Error())
			if err != nil {
				c.ErrorLogOnly("Encountered send error", err)
				return
			}

			// In case of failure, wait for next sent command.
			rawReq, err = c.Receive()
			if err != nil {
				c.ErrorLogOnly("Encountered receive error", err)
				return
			}

			// Go back to beginning of loop.
			continue
		}

		switch {

		case rawReq == "> done <":
			// TODO: Trigger state-dependent behaviour when user logged out.
			log.Printf("%s: done.", clientID)

		case rawReq == "> changed <":
			// TODO: Trigger state-dependent behaviour when session changed.
			log.Printf("%s: session changed.", clientID)

		case req.Command == "SELECT":
			if ok := worker.Select(c, req, clientID); ok {

				// If successful, signal end of operation to distributor.
				err := c.SignalSessionDone(nil)
				if err != nil {
					c.ErrorLogOnly("Encountered send error", err)
					return
				}
			}

		default:
			// Client sent inappropriate command. Signal tagged error.
			err := c.Send(fmt.Sprintf("%s BAD Received invalid IMAP command", req.Tag))
			if err != nil {
				c.ErrorLogOnly("Encountered send error", err)
				return
			}

			err = c.SignalSessionDone(nil)
			if err != nil {
				c.ErrorLogOnly("Encountered send error", err)
				return
			}
		}

		// Receive next incoming client command.
		rawReq, err = c.Receive()
		if err != nil {
			c.ErrorLogOnly("Encountered receive error", err)
			return
		}
	}

	log.Println("DISTRIBUTOR sent '> done <'")
}
