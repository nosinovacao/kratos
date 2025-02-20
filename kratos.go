package kratos

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/gorilla/websocket"
	"github.com/xmidt-org/webpa-common/device"
	"github.com/xmidt-org/webpa-common/logging"
	"github.com/xmidt-org/wrp-go/wrp"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 300 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 2048

	StatusDeviceDisconnected int = 523
	StatusDeviceTimeout      int = 524
)

// ClientFactory is used to generate a client by calling new on this type
type ClientFactory struct {
	DeviceName     string
	FirmwareName   string
	ModelName      string
	Manufacturer   string
	DestinationURL string
	CRT            string
	Key            string
	Handlers       []HandlerRegistry
	HandlePingMiss HandlePingMiss
	ClientLogger   log.Logger
}

// New is used to create a new kratos Client from a ClientFactory
func (f *ClientFactory) New() (Client, error) {
	inHeader := &clientHeader{
		deviceName:   f.DeviceName,
		firmwareName: f.FirmwareName,
		modelName:    f.ModelName,
		manufacturer: f.Manufacturer,
	}

	newConnection, connectionURL, err := createConnection(inHeader, f.DestinationURL, f.CRT, f.Key)

	if err != nil {
		return nil, err
	}

	newConnection.SetReadLimit(maxMessageSize)
	_ = newConnection.SetReadDeadline(time.Now().Add(pongWait))
	newConnection.SetPongHandler(func(string) error { _ = newConnection.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	// at this point we know that the URL connection is legitimate, so we can do some string manipulation
	// with the knowledge that `:` will be found in the string twice
	//connectionURL = connectionURL[len("ws://"):strings.LastIndex(connectionURL, ":")]
	myPingMissHandler := pingHandler{
		conn:           newConnection,
		handlePingMiss: f.HandlePingMiss,
		stop:           make(chan bool),
	}

	newClient := &client{
		deviceID:        inHeader.deviceName,
		userAgent:       "WebPA-1.6(" + inHeader.firmwareName + ";" + inHeader.modelName + "/" + inHeader.manufacturer + ";)",
		deviceProtocols: "TODO-what-to-put-here",
		hostname:        connectionURL,
		handlers:        f.Handlers,
		connection:      newConnection,
		headerInfo:      inHeader,
		pingHandler:     myPingMissHandler,
	}

	if f.ClientLogger != nil {
		newClient.Logger = f.ClientLogger
		myPingMissHandler.Logger = f.ClientLogger
	} else {
		newClient.Logger = logging.DefaultLogger()
		myPingMissHandler.Logger = logging.DefaultLogger()
	}

	for i := range newClient.handlers {
		newClient.handlers[i].keyRegex, err = regexp.Compile(newClient.handlers[i].HandlerKey)
		if err != nil {
			return nil, err
		}
	}

	go myPingMissHandler.checkPing(newClient)
	go newClient.read()

	return newClient, nil
}

// HandlePingMiss is a function called when we run into situations where we're not getting anymore pings
// the implementation of this function needs to be handled by the user of kratos
type HandlePingMiss func() error

type pingHandler struct {
	conn           *websocket.Conn
	handlePingMiss HandlePingMiss
	log.Logger
	stop chan bool
}

func (pmh *pingHandler) stopPingHandler() {
	pmh.stop <- true
}

func (pmh *pingHandler) checkPing(inClient *client) {
	pingTimer := time.NewTimer(pingPeriod)
	defer func() {
		pingTimer.Stop()
		pmh.conn.Close()
		close(pmh.stop)
	}()

	for {
		select {
		case <-pmh.stop:
			logging.Info(pmh).Log(logging.MessageKey(), "Stopping ping handler!")
			pmh.conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		case <-pingTimer.C:
			pmh.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := pmh.conn.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
				return
			}
		}
	}
}

// Client is what function calls we expose to the user of kratos
type Client interface {
	Hostname() string
	Send(message interface{}) error
	Close() error
}

type websocketConnection interface {
	WriteMessage(messageType int, data []byte) error
	ReadMessage() (messageType int, p []byte, err error)
	Close() error
}

// ReadHandler should be implemented by the user so that they
// may deal with received messages how they please
type ReadHandler interface {
	HandleMessage(msg interface{})
}

// HandlerRegistry is an internal data type for Client interface
// that helps keep track of registered handler functions
type HandlerRegistry struct {
	HandlerKey string
	keyRegex   *regexp.Regexp
	Handler    ReadHandler
}

type client struct {
	deviceID        string
	userAgent       string
	deviceProtocols string
	hostname        string
	handlers        []HandlerRegistry
	connection      websocketConnection
	headerInfo      *clientHeader
	pingHandler     pingHandler
	log.Logger
}

// used to track everything that we want to know about the client headers
type clientHeader struct {
	deviceName   string
	firmwareName string
	modelName    string
	manufacturer string
}

func (c *client) Hostname() string {
	return c.hostname
}

// used to open a channel for writing to servers
func (c *client) Send(message interface{}) (err error) {
	logging.Info(c).Log(logging.MessageKey(), "Sending message...")

	var buffer bytes.Buffer

	if err = wrp.NewEncoder(&buffer, wrp.Msgpack).Encode(message); err == nil {
		err = c.connection.WriteMessage(websocket.BinaryMessage, buffer.Bytes())
	}
	return
}

// will close the connection to the server
func (c *client) Close() (err error) {
	logging.Info(c).Log("Closing client...")
	c.pingHandler.stopPingHandler()
	return
}

// going to be used to access the HandleMessage() function
func (c *client) read() (err error) {
	logging.Info(c).Log("Reading message...")
	defer c.connection.Close()

	for {
		var serverMessage []byte
		_, serverMessage, err = c.connection.ReadMessage()
		if err != nil {
			return
		}

		// decode the message so we can read it
		wrpData := wrp.Message{}
		err = wrp.NewDecoderBytes(serverMessage, wrp.Msgpack).Decode(&wrpData)

		if err != nil {
			return
		}

		for i := 0; i < len(c.handlers); i++ {
			if c.handlers[i].keyRegex.MatchString(wrpData.Destination) {
				c.handlers[i].Handler.HandleMessage(wrpData)
			}
		}
	}
}

// private func used to generate the client that we're looking to produce
func createConnection(headerInfo *clientHeader, httpURL string, crtFile string, keyFile string) (connection *websocket.Conn, wsURL string, err error) {
	_, err = device.ParseID(headerInfo.deviceName)

	if err != nil {
		return nil, "", err
	}

	// make a header and put some data in that (including MAC address)
	// TODO: find special function for user agent
	headers := make(http.Header)
	headers.Add("X-Webpa-Device-Name", headerInfo.deviceName)
	headers.Add("X-Webpa-Firmware-Name", headerInfo.firmwareName)
	headers.Add("X-Webpa-Model-Name", headerInfo.modelName)
	headers.Add("X-Webpa-Manufacturer", headerInfo.manufacturer)

	var client http.Client
	var dialer websocket.Dialer

	if crtFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(crtFile, keyFile)
		if err != nil {
			return nil, "", err
		}

		tlsConfig := &tls.Config{Certificates: []tls.Certificate{cert}}

		transport := http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 300 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout: 5 * time.Second,
			TLSClientConfig:     tlsConfig,
		}

		dialer = websocket.Dialer{
			TLSClientConfig:  tlsConfig,
			HandshakeTimeout: 10 * time.Second,
			ReadBufferSize:   65535,
			WriteBufferSize:  65535,
		}

		client = http.Client{
			Transport: &transport,
		}
	}

	req, err := http.NewRequest("GET", httpURL, nil)
	req.Header.Set("X-Webpa-Device-Name", headerInfo.deviceName)
	resp, err := client.Do(req)
	req.Close = true

	if err != nil {
		return nil, "", err
	}

	defer resp.Body.Close()

	if resp.Request.Response == nil {
		return nil, "", errors.New("Petasos response is nil!")
	}
	
	if resp.StatusCode == http.StatusTemporaryRedirect || resp.Request.Response.StatusCode == http.StatusTemporaryRedirect {
		var location string

		if location = resp.Header.Get("Location"); location != "" {
			wsURL = strings.Replace(location, "http", "ws", 1) + "/api/v2/device"
		} else {
			location = resp.Request.Response.Header.Get("Location")
			wsURL = strings.Replace(location, "http", "ws", 1) + "/api/v2/device"
		}

		//Get url to which we are redirected and reconfigure it
		connection, resp, err = dialer.Dial(wsURL, headers)

		if err != nil {
			return nil, "", err
		}

		if resp == nil {
			return nil, "", err
		}

		defer resp.Body.Close()
	} else {
		if resp != nil {
			err = createError(resp, fmt.Errorf("Received invalid response from petasos!"))
		}
		return nil, "", err
	}

	return connection, wsURL, nil
}

type Message struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (msg Message) String() string {
	return fmt.Sprintf("%d:%s", msg.Code, msg.Message)
}

type Error struct {
	Message  Message
	SubError error
}

func createError(resp *http.Response, err error) *Error {
	var msg Message
	defer resp.Body.Close()
	data, _ := ioutil.ReadAll(resp.Body)
	json.Unmarshal(data, &msg)

	if msg.Message == "" {
		switch resp.StatusCode {
		case StatusDeviceDisconnected:
			msg.Message = "ErrorDeviceBusy"
		case StatusDeviceTimeout:
			msg.Message = "ErrorTransactionsClosed/ErrorTransactionsAlreadyClosed/ErrorDeviceClosed"
		default:
			msg.Message = http.StatusText(msg.Code)
		}
	}

	return &Error{
		Message:  msg,
		SubError: err,
	}
}

func (e *Error) Error() string {
	return fmt.Sprintf("message: %s with error: %s", e.Message, e.SubError.Error())
}
