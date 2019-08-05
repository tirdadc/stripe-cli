package logs

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/logrusorgru/aurora"
	log "github.com/sirupsen/logrus"

	"github.com/stripe/stripe-cli/pkg/ansi"
	"github.com/stripe/stripe-cli/pkg/stripeauth"
	"github.com/stripe/stripe-cli/pkg/websocket"
)

const outputFormatJSON = "json"

// Config provides the cfguration of a Proxy
type Config struct {
	APIBaseURL string

	// DeviceName is the name of the device sent to Stripe to help identify the device
	DeviceName string

	// Key is the API key used to authenticate with Stripe
	Key string

	Log *log.Logger

	// Force use of unencrypted ws:// protocol instead of wss://
	NoWSS bool

	// Output format for request logs
	OutputFormat string

	// WebSocketFeature is the feature specified for the websocket connection
	WebSocketFeature string
}

// Tailer is the main interface for running the log tailing session
type Tailer struct {
	cfg *Config

	stripeAuthClient *stripeauth.Client
	webSocketClient  *websocket.Client

	interruptCh chan os.Signal
}

// EventPayload is the mapping for fields in event payloads from request log tailing
type EventPayload struct {
	CreatedAt string `json:"created_at"`
	Method    string `json:"method"`
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
	URL       string `json:"url"`
}

// New creates a new Tailer
func New(cfg *Config) *Tailer {
	if cfg.Log == nil {
		cfg.Log = &log.Logger{Out: ioutil.Discard}
	}
	return &Tailer{
		cfg: cfg,
		stripeAuthClient: stripeauth.NewClient(cfg.Key, &stripeauth.Config{
			Log:        cfg.Log,
			APIBaseURL: cfg.APIBaseURL,
		}),
		interruptCh: make(chan os.Signal, 1),
	}
}


// Run sets the websocket connection
func (tailer *Tailer) Run() error {
	s := ansi.StartSpinner("Getting ready...", tailer.cfg.Log.Out)

	// Intercept Ctrl+c so we can do some clean up
	signal.Notify(tailer.interruptCh, os.Interrupt, syscall.SIGTERM)

	session, err := tailer.stripeAuthClient.Authorize(tailer.cfg.DeviceName, tailer.cfg.WebSocketFeature)
	if err != nil {
		// TODO: better error handling / retries
		tailer.cfg.Log.Fatalf("Error while authenticating with Stripe: %v", err)
	}

	tailer.webSocketClient = websocket.NewClient(
		session.WebSocketURL,
		session.WebSocketID,
		session.WebSocketAuthorizedFeature,
		&websocket.Config{
			Log:                 tailer.cfg.Log,
			NoWSS:               tailer.cfg.NoWSS,
			ReconnectInterval:   time.Duration(session.ReconnectDelay) * time.Second,
			EventHandler: websocket.EventHandlerFunc(tailer.processRequestLogEvent),
		},
	)
	go tailer.webSocketClient.Run()

	ansi.StopSpinner(s, "Ready! You're now waiting to receive API request logs (^C to quit)", tailer.cfg.Log.Out)

	for {
		select {
		case <-tailer.interruptCh:
			log.WithFields(log.Fields{
				"prefix": "logs.Tailer.Run",
			}).Debug("Ctrl+C received, cleaning up...")

			if tailer.webSocketClient != nil {
				tailer.webSocketClient.Stop()
			}

			log.WithFields(log.Fields{
				"prefix": "logs.Tailer.Run",
			}).Debug("Bye!")

			return nil
		}
	}
}

func (tailer *Tailer) processRequestLogEvent(msg websocket.IncomingMessage) {
	if msg.RequestLogEvent == nil {
		tailer.cfg.Log.Warn("WebSocket specified for request logs received non-request-logs event")
		return
	}

	requestLogEvent := msg.RequestLogEvent

	tailer.cfg.Log.WithFields(log.Fields{
		"prefix":     "logs.Tailer.processRequestLogEvent",
		"webhook_id": requestLogEvent.RequestLogID,
	}).Debugf("Processing request log event")

	if tailer.cfg.OutputFormat == outputFormatJSON {
		fmt.Println(ansi.ColorizeJSON(requestLogEvent.EventPayload, os.Stdout))
		return
	}

	var payload EventPayload
	if err := json.Unmarshal([]byte(requestLogEvent.EventPayload), &payload); err != nil {
		tailer.cfg.Log.Warn("Received malformed payload: ", err)
	}

	coloredStatus := colorizeStatus(payload.Status)

	outputStr := fmt.Sprintf("%s [%d] %s %s %s", payload.CreatedAt, coloredStatus, payload.Method, payload.URL, payload.RequestID)
	fmt.Println(outputStr)
}

func colorizeStatus(status int) aurora.Value {
	color := ansi.Color(os.Stdout)

	if status >= 500 {
		return color.Red(status).Bold()
	} else if status >= 400 {
		return color.Yellow(status).Bold()
	} else {
		return color.Green(status).Bold()
	}
}
