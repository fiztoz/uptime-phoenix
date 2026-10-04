package probe

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// EdgeReadiness contains local execution diagnostics. Hub reachability does not
// determine readiness: an enrolled probe continues its accepted work offline.
type EdgeReadiness struct {
	Ready            bool    `json:"ready"`
	DBWritable       bool    `json:"db_writable"`
	SchedulerHealthy bool    `json:"scheduler_healthy"`
	ConfigRevision   Decimal `json:"config_revision"`
}

// EdgeSessionHandler starts negotiation on an authenticated runtime socket.
// It must honor cancellation, bound its I/O, and return when the socket closes.
type EdgeSessionHandler func(context.Context, *websocket.Conn, domain.EdgeEnrollment) error

// NewEdgeHTTPHandler exposes only TLS probe sockets and bounded health checks.
// There is no hub auth, admin, public status page or embedded frontend route.
func NewEdgeHTTPHandler(identity *RuntimeIdentity, enrollment *services.EdgeEnrollmentService, runtime EdgeSessionHandler, health func(context.Context) EdgeReadiness) (*echo.Echo, error) {
	return newEdgeHTTPHandler(identity, enrollment, runtime, health, nil)
}

// NewManagedEdgeHTTPHandler binds runtime admission to the actual TLS selection.
// Install the manager's TLSConfig and ConnContext on the owning HTTP server.
func NewManagedEdgeHTTPHandler(identity *RuntimeIdentity, enrollment *services.EdgeEnrollmentService, runtime EdgeSessionHandler, health func(context.Context) EdgeReadiness, manager *EdgeTLSManager) (*echo.Echo, error) {
	if manager == nil {
		return nil, errors.New("edge server requires a TLS manager")
	}
	return newEdgeHTTPHandler(identity, enrollment, runtime, health, manager)
}

func newEdgeHTTPHandler(identity *RuntimeIdentity, enrollment *services.EdgeEnrollmentService, runtime EdgeSessionHandler, health func(context.Context) EdgeReadiness, manager *EdgeTLSManager) (*echo.Echo, error) {
	if identity == nil || len(identity.Certificate.Certificate) == 0 || identity.Certificate.Leaf == nil || enrollment == nil || runtime == nil || health == nil {
		return nil, errors.New("edge server requires identity, authentication, runtime and health dependencies")
	}
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.HTTPErrorHandler = func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		code := http.StatusInternalServerError
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) {
			code = httpErr.Code
		}
		_ = c.JSON(code, struct {
			Error string `json:"error"`
		}{Error: http.StatusText(code)})
	}
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, struct {
			Alive bool `json:"alive"`
		}{Alive: true})
	})
	e.GET("/readyz", func(c echo.Context) error {
		ctx, cancel := context.WithTimeout(c.Request().Context(), 2*time.Second)
		defer cancel()
		diagnostic := health(ctx)
		diagnostic.Ready = diagnostic.Ready && diagnostic.DBWritable && diagnostic.SchedulerHealthy && diagnostic.ConfigRevision > 0
		status := http.StatusOK
		if !diagnostic.Ready {
			status = http.StatusServiceUnavailable
		}
		return c.JSON(status, diagnostic)
	})
	// Bound concurrent handshake allocations independently of network keepalive.
	slots := make(chan struct{}, 8)
	e.GET("/ws/probe/enroll/v1", func(c echo.Context) error {
		token, err := edgeBearer(c.Request(), "/ws/probe/enroll/v1")
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized)
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			return echo.NewHTTPError(http.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
		defer cancel()
		valid, err := enrollment.AuthenticateEnrollment(ctx, token, time.Now().UTC())
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable)
		}
		if !valid {
			return echo.NewHTTPError(http.StatusUnauthorized)
		}
		conn, err := acceptEdgeSocket(c)
		if err != nil {
			return nil
		}
		defer func() { _ = conn.CloseNow() }()
		kind, data, err := conn.Read(ctx)
		if err != nil || kind != websocket.MessageText {
			return nil
		}
		envelope, request, err := DecodeEnrollRequest(data)
		if err != nil || envelope.ConnectionGeneration != 0 || request.ProbeID != identity.ProbeID {
			return nil
		}
		at := time.Now().UTC()
		binding := domain.EdgeEnrollment{HubID: request.HubID, ProbeID: request.ProbeID, EnrollmentID: request.EnrollmentID, CredentialVersion: int64(request.CredentialVersion)}
		if err := enrollment.Accept(ctx, token, request.Token, binding, at); err != nil {
			return nil
		}
		appliedAt, expiry := Timestamp(at), Timestamp(identity.Certificate.Leaf.NotAfter)
		pin := identity.Fingerprint
		if manager != nil {
			var notAfter time.Time
			pin, notAfter, err = manager.CurrentIdentity(ctx)
			if err != nil {
				return nil
			}
			expiry = Timestamp(notAfter)
		}
		frame, err := encodeFrame("enroll.result", 0, EnrollResult{HubID: request.HubID, ProbeID: request.ProbeID, EnrollmentID: request.EnrollmentID, Status: "applied", CredentialVersion: request.CredentialVersion, AppliedAt: &appliedAt, TLSFingerprint: &pin, CertificateNotAfter: &expiry, Message: "Enrollment applied"})
		if err != nil {
			return nil
		}
		_ = conn.Write(ctx, websocket.MessageText, frame)
		return nil
	})
	e.GET("/ws/probe/v1", func(c echo.Context) error {
		token, err := edgeBearer(c.Request(), "/ws/probe/v1")
		if err != nil {
			return echo.NewHTTPError(http.StatusUnauthorized)
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			return echo.NewHTTPError(http.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
		binding, valid, err := enrollment.AuthenticateRuntime(ctx, token)
		cancel()
		if err != nil {
			return echo.NewHTTPError(http.StatusServiceUnavailable)
		}
		if !valid {
			return echo.NewHTTPError(http.StatusUnauthorized)
		}
		if manager != nil {
			binding, err = manager.bindEnrollment(c.Request().Context(), binding)
			if err != nil {
				return echo.NewHTTPError(http.StatusServiceUnavailable)
			}
		}
		conn, err := acceptEdgeSocket(c)
		if err != nil {
			return nil
		}
		defer func() { _ = conn.CloseNow() }()
		_ = runtime(c.Request().Context(), conn, binding)
		return nil
	})
	return e, nil
}

func edgeBearer(r *http.Request, path string) (string, error) {
	if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || r.URL.Path != path || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Origin") != "" || r.Header.Get("Sec-WebSocket-Protocol") != "phoenix.probe.v1" {
		return "", errors.New("invalid probe transport")
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 280 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", errors.New("invalid probe authorization")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("invalid probe authorization")
	}
	return token, nil
}

func acceptEdgeSocket(c echo.Context) (*websocket.Conn, error) {
	conn, err := websocket.Accept(c.Response().Writer, c.Request(), &websocket.AcceptOptions{Subprotocols: []string{"phoenix.probe.v1"}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(MaxFrameBytes)
	return conn, nil
}

func encodeFrame(kind string, generation Decimal, payload any) ([]byte, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("allocate frame identity failed")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.New("encode frame payload failed")
	}
	frame, err := json.Marshal(Envelope{ProtocolVersion: 1, Type: kind, MessageID: id.String(), SentAt: Timestamp(time.Now().UTC()), ConnectionGeneration: generation, Payload: data})
	if err != nil || len(frame) > MaxFrameBytes {
		return nil, errors.New("invalid frame encoding or size")
	}
	return frame, nil
}
