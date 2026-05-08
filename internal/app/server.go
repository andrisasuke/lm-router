package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type ServerState string

const (
	ServerOff      ServerState = "OFF"
	ServerStarting ServerState = "STARTING"
	ServerOn       ServerState = "ON"
	ServerError    ServerState = "ERROR"
)

type ServerStatus struct {
	State    ServerState
	Host     string
	Port     int
	Endpoint string
	Error    string
}

type ServerControllerConfig struct {
	HandlerFactory func() (http.Handler, error)
	Logger         Logger
}

type ServerController struct {
	mu             sync.Mutex
	handlerFactory func() (http.Handler, error)
	logger         Logger
	server         *http.Server
	status         ServerStatus
}

func NewServerController(cfg ServerControllerConfig) *ServerController {
	return &ServerController{
		handlerFactory: cfg.HandlerFactory,
		logger:         cfg.Logger,
		status:         ServerStatus{State: ServerOff},
	}
}

func (c *ServerController) Start(ctx context.Context, host string, port int) error {
	c.mu.Lock()
	if c.server != nil {
		c.mu.Unlock()
		return nil
	}
	c.status = ServerStatus{State: ServerStarting, Host: host, Port: port}
	c.mu.Unlock()

	handler, err := c.handlerFactory()
	if err != nil {
		c.setError(host, port, err)
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		c.setError(host, port, err)
		return err
	}
	actualPort := listener.Addr().(*net.TCPAddr).Port
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	endpointHost := host
	if endpointHost == "0.0.0.0" || endpointHost == "::" || endpointHost == "" {
		endpointHost = "127.0.0.1"
	}
	status := ServerStatus{
		State:    ServerOn,
		Host:     host,
		Port:     actualPort,
		Endpoint: fmt.Sprintf("http://%s:%d", endpointHost, actualPort),
	}
	c.mu.Lock()
	c.server = srv
	c.status = status
	c.mu.Unlock()
	if c.logger != nil {
		c.logger.Printf("[server] listening on %s", status.Endpoint)
	}
	go func() {
		err := srv.Serve(listener)
		if err != nil && err != http.ErrServerClosed {
			c.setError(host, actualPort, err)
		}
	}()
	return nil
}

func (c *ServerController) Stop(ctx context.Context) error {
	c.mu.Lock()
	srv := c.server
	c.server = nil
	c.status = ServerStatus{State: ServerOff}
	c.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if c.logger != nil {
		c.logger.Printf("[server] stopped")
	}
	return err
}

func (c *ServerController) Restart(ctx context.Context, host string, port int) error {
	if err := c.Stop(ctx); err != nil {
		return err
	}
	return c.Start(ctx, host, port)
}

func (c *ServerController) Status() ServerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *ServerController) setError(host string, port int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.server = nil
	c.status = ServerStatus{State: ServerError, Host: host, Port: port, Error: err.Error()}
	if c.logger != nil {
		c.logger.Printf("[server] error host=%s port=%d error=%s", host, port, err)
	}
}
