package wattpilot

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/pbkdf2"
)

const (
	CONTEXT_TIMEOUT   = 30 // seconds
	RECONNECT_TIMEOUT = 5  // seconds
)

//go:generate go run gen/generate.go

type eventFunc func(map[string]interface{})

type Wattpilot struct {
	_requestId    int64
	connected     chan bool
	initialized   chan bool
	_secured      bool
	_name         string
	_hostname     string
	_serial       string
	_version      string
	_manufacturer string
	_devicetype   string
	_protocol     float64
	_readContext  context.Context
	_readCancel   context.CancelFunc
	_readMutex    sync.Mutex

	_token3         string
	_hashedpassword string
	_host           string
	_password       string
	_isInitialized  bool
	_isConnected    bool
	_status         map[string]interface{}
	_eventHandler   map[string]eventFunc

	_sendResponse chan string
	_interrupt    chan os.Signal
	_done         chan interface{}

	_notifications     *Pubsub
	_log               *log.Logger
	_currentConnection *net.Conn
}

func New(host string, password string) *Wattpilot {

	w := &Wattpilot{
		_host:     host,
		_password: password,

		connected:     make(chan bool),
		initialized:   make(chan bool),
		_sendResponse: make(chan string),
		_done:         make(chan interface{}),
		_interrupt:    make(chan os.Signal),

		_currentConnection: nil,
		_isConnected:       false,
		_isInitialized:     false,
		_requestId:         0,
		_status:            make(map[string]interface{}),
	}

	w._readContext, w._readCancel = context.WithCancel(context.Background())

	w._log = log.New()
	w._log.SetFormatter(&log.JSONFormatter{})
	w._log.SetLevel(log.ErrorLevel)
	if level := os.Getenv("WATTPILOT_LOG"); level != "" {
		if err := w.ParseLogLevel(level); err != nil {
			w._log.Warn("Could not parse log level setting ", err)
		}
	}

	signal.Notify(w._interrupt, os.Interrupt) // Notify the interrupt channel for SIGINT

	w._notifications = NewPubsub()

	w._eventHandler = map[string]eventFunc{
		"hello":          w.onEventHello,
		"authRequired":   w.onEventAuthRequired,
		"response":       w.onEventResponse,
		"authSuccess":    w.onEventAuthSuccess,
		"authError":      w.onEventAuthError,
		"fullStatus":     w.onEventFullStatus,
		"deltaStatus":    w.onEventDeltaStatus,
		"clearInverters": w.onEventClearInverters,
		"updateInverter": w.onEventUpdateInverter,
	}

	go w.processLoop(context.Background())

	return w

}
func (w *Wattpilot) SetLogLevel(level log.Level) {
	w._log.SetLevel(level)
}

func (w *Wattpilot) ParseLogLevel(level string) error {
	loglevel, err := log.ParseLevel(level)
	if err != nil {
		return err
	}
	w._log.SetLevel(loglevel)
	return nil
}

func (w *Wattpilot) GetName() string {
	return w._name
}

func (w *Wattpilot) GetSerial() string {
	return w._serial
}

func (w *Wattpilot) GetHost() string {
	return w._host
}

func (w *Wattpilot) IsInitialized() bool {
	return w._isInitialized
}

func (w *Wattpilot) Properties() []string {
	keys := []string{}

	w._readMutex.Lock()
	defer w._readMutex.Unlock()

	for k := range w._status {
		keys = append(keys, k)
	}
	return keys
}
func (w *Wattpilot) Alias() []string {
	keys := []string{}
	for k := range propertyMap {
		keys = append(keys, k)
	}
	return keys
}
func (w *Wattpilot) LookupAlias(name string) string {
	return propertyMap[name]
}

func (w *Wattpilot) getRequestId() int64 {
	return atomic.AddInt64(&w._requestId, 1)
}

func (w *Wattpilot) onEventHello(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Hello from Wattpilot")

	if hasKey(message, "hostname") {
		w._hostname = message["hostname"].(string)
	}
	if hasKey(message, "friendly_name") {
		w._name = message["friendly_name"].(string)
	} else {
		w._name = w._hostname
	}
	w._serial = message["serial"].(string)
	if hasKey(message, "version") {
		w._version = message["version"].(string)
	}
	w._manufacturer = message["manufacturer"].(string)
	w._devicetype = message["devicetype"].(string)
	w._protocol = message["protocol"].(float64)
	if hasKey(message, "secured") {
		w._secured = message["secured"].(bool)
	}

	pwd_data := pbkdf2.Key([]byte(w._password), []byte(w._serial), 100000, 256, sha512.New)
	w._hashedpassword = base64.StdEncoding.EncodeToString([]byte(pwd_data))[:32]

}

func (w *Wattpilot) onEventAuthRequired(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Auhtentication required")

	token1 := message["token1"].(string)
	token2 := message["token2"].(string)

	w._token3 = randomHexString(32)
	hash1 := sha256sum(token1 + w._hashedpassword)
	hash := sha256sum(w._token3 + token2 + hash1)
	response := map[string]interface{}{
		"type":   "auth",
		"token3": w._token3,
		"hash":   hash,
	}
	err := w.onSendResponse(false, response)
	w._isInitialized = (err != nil)
}

func (w *Wattpilot) onSendResponse(secured bool, message map[string]interface{}) error {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Sending data to wattpilot: ", message["requestId"], " secured: ", secured)

	if secured {
		msgId := message["requestId"].(int64)
		payload, _ := json.Marshal(message)

		mac := hmac.New(sha256.New, []byte(w._hashedpassword))
		mac.Write(payload)
		message = make(map[string]interface{})
		message["type"] = "securedMsg"
		message["data"] = string(payload)
		message["requestId"] = fmt.Sprintf("%d", msgId) + "sm"
		message["hmac"] = hex.EncodeToString(mac.Sum(nil))
	}

	data, _ := json.Marshal(message)
	err := wsutil.WriteClientMessage(*w._currentConnection, ws.OpText, data)
	if err != nil {
		return err
	}
	return nil
}

func (w *Wattpilot) onEventResponse(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Response on Event ", message["type"])

	mType := message["type"].(string)
	success, ok := message["success"]
	if ok && success.(bool) {
		return
	}
	if !success.(bool) {
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Error("Failure happened: ", message["message"])
		return
	}
	if mType == "response" {
		w._sendResponse <- message["message"].(string)
		return
	}
}

func (w *Wattpilot) onEventAuthSuccess(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Auhtentication successful")
	w.connected <- true

}

func (w *Wattpilot) onEventAuthError(message map[string]interface{}) {
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Error("Auhtentication error", message)
	w.connected <- false
}

func (w *Wattpilot) onEventFullStatus(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Full status update - is partial: ", message["partial"])

	isPartial := message["partial"].(bool)

	w.updateStatus(message)

	if isPartial {
		return
	}
	if w.IsInitialized() {
		return
	}

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Initialization done")

	w.initialized <- true
	w._isInitialized = true
}
func (w *Wattpilot) onEventDeltaStatus(message map[string]interface{}) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Delta status update")
	w.updateStatus(message)

}

func (w *Wattpilot) updateStatus(message map[string]interface{}) {

	statusUpdates := message["status"].(map[string]interface{})
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Data-status gets updates #", len(statusUpdates))

	w._readMutex.Lock()
	defer w._readMutex.Unlock()

	for k, v := range statusUpdates {
		w._status[k] = v
		go w._notifications.Publish(k, v)
	}
}

func (w *Wattpilot) GetNotifications(prop string) <-chan interface{} {
	return w._notifications.Subscribe(prop)
}

func (w *Wattpilot) onEventClearInverters(message map[string]interface{}) {
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("clear inverters")
}
func (w *Wattpilot) onEventUpdateInverter(message map[string]interface{}) {
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("update inverters")
}
func (w *Wattpilot) Disconnect() {
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Going to disconnect...")
	w._isConnected = false
	w.disconnectImpl()
	<-w._interrupt
}

func (w *Wattpilot) disconnectImpl() {
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Disconnecting...")

	if !w._isInitialized {
		return
	}

	if err := (*w._currentConnection).Close(); err != nil {
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Error on closing connection: ", err)
	}

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("closed connection")

	w._isInitialized = false
	w._isConnected = false
	w._currentConnection = nil
	w._status = make(map[string]interface{})

}

func (w *Wattpilot) Connect() error {

	if w._isConnected || w._isInitialized {
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Debug("Already Connected")
		return nil
	}

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Connecting")

	var err error
	dialContext, cancel := context.WithTimeout(w._readContext, time.Second*CONTEXT_TIMEOUT)
	defer cancel()
	conn, reader, _, err := ws.DefaultDialer.Dial(dialContext, fmt.Sprintf("ws://%s/ws", w._host))
	if err != nil {
		return err
	}
	w._currentConnection = &conn
	if reader != nil {
		ws.PutReader(reader)
	}
	go w.receiveHandler(w._readContext)

	w._isConnected = <-w.connected
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Connection is ", w._isConnected)
	if !w._isConnected {
		return errors.New("could not connect")
	}

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Connected - waiting for initializiation...")

	<-w.initialized

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Connected - and initializiated")

	return nil
}

func (w *Wattpilot) reconnect() {

	if w._isConnected && !w._isInitialized {
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Reconnect - Is still connected")
		return
	}

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Debug("Reconnecting..")
	time.Sleep(time.Second * time.Duration(RECONNECT_TIMEOUT))
	if err := w.Connect(); err != nil {
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Debug("Reconnect failure: ", err)
		return
	}
	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Successfully reconnected")

}

func (w *Wattpilot) processLoop(ctx context.Context) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Starting processing loop...")
	delayDuration := time.Duration(time.Second * CONTEXT_TIMEOUT)
	delay := time.NewTimer(delayDuration)

	for {
		select {
		case <-delay.C:
			delay.Reset(delayDuration)
			if !w._isInitialized {
				w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("No Hello there")
				continue
			}
			w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Hello there")
			go func() {
				time.Sleep(time.Millisecond * 100)
				if err := w.RequestStatusUpdate(); err != nil {
					w._log.WithFields(log.Fields{"wattpilot": w._host}).Error("Full Status Update failed: ", err)
					w.disconnectImpl()
					w.reconnect()
				}
			}()
			break
		case <-w._readContext.Done():
			w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Read context is done")
			w.disconnectImpl()
			w.reconnect()
			break

		case <-ctx.Done():
		case <-w._interrupt:
			w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("Stopping process loop...")
			w.disconnectImpl()
			if !delay.Stop() {
				<-delay.C
			}
			return
		}
	}
}

func (w *Wattpilot) receiveHandler(ctx context.Context) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Starting receive handler...")

	for {
		msg, err := wsutil.ReadServerText(*w._currentConnection)
		if err != nil {
			// w._readCancel()
			w._log.WithFields(log.Fields{"wattpilot": w._host}).Info("Stopping receive handler...")
			return
		}
		data := make(map[string]interface{})
		err = json.Unmarshal(msg, &data)
		if err != nil {
			continue
		}
		msgType, isTypeAvailable := data["type"]
		if !isTypeAvailable {
			continue
		}
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("receiving ", msgType)

		funcCall, isKnown := w._eventHandler[msgType.(string)]
		if !isKnown {
			continue
		}
		funcCall(data)
		w._log.WithFields(log.Fields{"wattpilot": w._host}).Trace("done ", msgType)
	}

}

func (w *Wattpilot) GetProperty(name string) (interface{}, error) {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Debug("Get Property ", name)

	if !w._isInitialized {
		return nil, errors.New("connection is not valid")
	}

	origName := name
	if v, isKnown := propertyMap[name]; isKnown {
		name = v
	}
	m, post := PostProcess[origName]
	if post {
		name = m.key
	}

	w._readMutex.Lock()
	defer w._readMutex.Unlock()

	if !hasKey(w._status, name) {
		return nil, errors.New("could not find value of " + name)
	}
	value := w._status[name]
	if post {
		value, _ = m.f(value)
	}
	return value, nil
}

func (w *Wattpilot) SetProperty(name string, value interface{}) error {

	w._log.WithFields(log.Fields{"wattpilot": w._host}).Debug("setting property ", name, " to ", value)

	if !w._isInitialized {
		return errors.New("Connection is not valid")
	}

	w._readMutex.Lock()
	defer w._readMutex.Unlock()

	if !hasKey(w._status, name) {
		return errors.New("Could not find reference for update on " + name)
	}

	return w.sendUpdate(name, value)

}

func (w *Wattpilot) transformValue(value interface{}) interface{} {

	switch value := value.(type) {
	case int:
		return value
	case int64:
		return value
	case float64:
		return value
	}
	in_value := fmt.Sprintf("%v", value)
	if out_value, err := strconv.Atoi(in_value); err == nil {
		return out_value
	}
	if out_value, err := strconv.ParseBool(in_value); err == nil {
		return out_value
	}
	if out_value, err := strconv.ParseFloat(in_value, 64); err == nil {
		return out_value
	}

	return in_value
}

func (w *Wattpilot) sendUpdate(name string, value interface{}) error {

	message := make(map[string]interface{})
	message["type"] = "setValue"
	message["requestId"] = w.getRequestId()
	message["key"] = name
	message["value"] = w.transformValue(value)
	return w.onSendResponse(w._secured, message)

}

// --------------------------------
// helper functions that wrap properties
// --------------------------------

func (w *Wattpilot) StatusInfo() {

	fmt.Println("Wattpilot: " + w._name)
	fmt.Println("Serial: ", w._serial)

	v, _ := w.GetProperty("car")
	fmt.Printf("Car Connected: %v\n", v)
	v, _ = w.GetProperty("alw")
	fmt.Printf("Charge Status %v\n", v)
	v, _ = w.GetProperty("imo")
	fmt.Printf("Mode: %v\n", v)
	v, _ = w.GetProperty("amp")
	fmt.Printf("Power: %v\n\nCharge: ", v)

	v1, v2, v3, _ := w.GetVoltages()
	fmt.Printf("%v V, %v V, %v V", v1, v2, v3)
	fmt.Printf("\n\t")

	i1, i2, i3, _ := w.GetCurrents()
	fmt.Printf("%v A, %v A, %v A", i1, i2, i3)
	fmt.Printf("\n\t")

	for _, i := range []string{"power1", "power2", "power3"} {
		v, _ := w.GetProperty(i)
		fmt.Printf("%v W, ", v)
	}
	fmt.Println("")
}

func (w *Wattpilot) GetPower() (float64, error) {

	v, err := w.GetProperty("power")
	if err != nil {
		return -1, err
	}
	return strconv.ParseFloat(v.(string), 64)
}

func (w *Wattpilot) GetCurrents() (float64, float64, float64, error) {

	var currents []float64
	for _, i := range []string{"amps1", "amps2", "amps3"} {
		v, err := w.GetProperty(i)
		if err != nil {
			return -1, -1, -1, err
		}
		fi, err := strconv.ParseFloat(v.(string), 64)
		if err != nil {
			return -1, -1, -1, err
		}

		currents = append(currents, fi)
	}
	return currents[0], currents[1], currents[2], nil
}

func (w *Wattpilot) GetVoltages() (float64, float64, float64, error) {

	var voltages []float64
	for _, i := range []string{"voltage1", "voltage2", "voltage2"} {
		v, err := w.GetProperty(i)
		if err != nil {
			return -1, -1, -1, err
		}
		fi, err := strconv.ParseFloat(v.(string), 64)
		if err != nil {
			return -1, -1, -1, err
		}

		voltages = append(voltages, fi)
	}
	return voltages[0], voltages[1], voltages[2], nil
}

func (w *Wattpilot) SetCurrent(current float64) error {

	return w.SetProperty("amp", current)
}

func (w *Wattpilot) GetRFID() (string, error) {

	resp, err := w.GetProperty("trx")
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	rfid := resp.(float64)
	return fmt.Sprint(rfid), nil

}

func (w *Wattpilot) GetCarIdentifier() (string, error) {

	resp, err := w.GetProperty("cak")
	if err != nil {
		return "", err
	}
	if resp == nil {
		return "", nil
	}
	return resp.(string), nil

}

func (w *Wattpilot) RequestStatusUpdate() error {
	message := make(map[string]interface{})
	message["type"] = "requestFullStatus"
	message["requestId"] = w.getRequestId()
	return w.onSendResponse(w._secured, message)
}
