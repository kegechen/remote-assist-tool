//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const (
	eventIDServiceStarted = 1
	eventIDServiceStop    = 2
	eventIDServiceFailed  = 3
	eventIDLogInfo        = 100
	eventIDLogWarning     = 200
	eventIDLogError       = 300
)

type windowsRelayService struct {
	eventLog   windowsEventLogger
	configPath string
	runRelay   func(context.Context, []string, io.Writer, io.Writer, func()) error
}

type eventLogWriter struct {
	eventLog windowsEventLogger
	messages chan string
	done     chan struct{}
	mu       sync.RWMutex
	closed   bool
	once     sync.Once
	dropped  atomic.Uint64
}

type windowsEventLogger interface {
	Info(uint32, string) error
	Warning(uint32, string) error
	Error(uint32, string) error
}

func runWindowsService() error {
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		return err
	}
	eventLogger, err := eventlog.Open(windowsEventSource)
	if err != nil {
		return fmt.Errorf("open Windows Event Log source: %w", err)
	}
	defer eventLogger.Close()

	logWriter := newEventLogWriter(eventLogger, 1024)
	previousOutput := log.Writer()
	log.SetOutput(logWriter)
	defer log.SetOutput(previousOutput)
	defer logWriter.Close()
	return svc.Run(windowsServiceName, &windowsRelayService{
		eventLog:   eventLogger,
		configPath: paths.configFile,
		runRelay:   runRelayWithReady,
	})
}

func (service *windowsRelayService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending, CheckPoint: 1, WaitHint: 20_000}
	paths, err := defaultWindowsServicePaths()
	if err != nil {
		_ = service.eventLog.Error(eventIDServiceFailed, err.Error())
		return true, 1
	}
	config, err := loadWindowsServiceConfig(service.configPath, defaultWindowsServiceConfig(paths))
	if err != nil {
		_ = service.eventLog.Error(eventIDServiceFailed, err.Error())
		return true, 2
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverDone := make(chan error, 1)
	serverReady := make(chan struct{})
	go func() {
		serverDone <- service.runRelay(ctx, config.relayArgs(), io.Discard, io.Discard, func() { close(serverReady) })
	}()
	select {
	case err := <-serverDone:
		if err == nil {
			err = fmt.Errorf("relay stopped before becoming ready")
		}
		_ = service.eventLog.Error(eventIDServiceFailed, err.Error())
		return true, 3
	case <-serverReady:
	}

	runningStatus := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- runningStatus
	_ = service.eventLog.Info(eventIDServiceStarted, "Remote Assist Relay service started")

	for {
		select {
		case err := <-serverDone:
			if err != nil {
				_ = service.eventLog.Error(eventIDServiceFailed, "Relay stopped unexpectedly: "+err.Error())
				return true, 4
			}
			_ = service.eventLog.Info(eventIDServiceStop, "Remote Assist Relay service stopped")
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- runningStatus
			case svc.Stop, svc.Shutdown:
				statuses <- svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 30_000}
				_ = service.eventLog.Info(eventIDServiceStop, "Remote Assist Relay service stop requested")
				cancel()
				timer := time.NewTimer(30 * time.Second)
				select {
				case err := <-serverDone:
					timer.Stop()
					if err != nil {
						_ = service.eventLog.Error(eventIDServiceFailed, "Relay shutdown failed: "+err.Error())
						return true, 5
					}
					_ = service.eventLog.Info(eventIDServiceStop, "Remote Assist Relay service stopped")
					return false, 0
				case <-timer.C:
					_ = service.eventLog.Error(eventIDServiceFailed, "Relay shutdown timed out after 30 seconds")
					return true, 6
				}
			default:
				_ = service.eventLog.Warning(eventIDLogWarning, fmt.Sprintf("Unsupported service control request: %d", request.Cmd))
			}
		}
	}
}

func (writer *eventLogWriter) Write(data []byte) (int, error) {
	message := strings.TrimSpace(string(data))
	if message == "" {
		return len(data), nil
	}
	writer.mu.RLock()
	defer writer.mu.RUnlock()
	if writer.closed {
		return len(data), nil
	}
	select {
	case writer.messages <- message:
	default:
		writer.dropped.Add(1)
	}
	return len(data), nil
}

func newEventLogWriter(eventLogger windowsEventLogger, capacity int) *eventLogWriter {
	writer := &eventLogWriter{
		eventLog: eventLogger,
		messages: make(chan string, capacity),
		done:     make(chan struct{}),
	}
	go writer.run()
	return writer
}

func (writer *eventLogWriter) Close() {
	writer.once.Do(func() {
		writer.mu.Lock()
		writer.closed = true
		close(writer.messages)
		writer.mu.Unlock()
		<-writer.done
	})
}

func (writer *eventLogWriter) run() {
	defer close(writer.done)
	for message := range writer.messages {
		writer.writeMessage(message)
	}
	if dropped := writer.dropped.Load(); dropped != 0 {
		_ = writer.eventLog.Warning(eventIDLogWarning, fmt.Sprintf("Dropped %d Event Log messages because the queue was full", dropped))
	}
}

func (writer *eventLogWriter) writeMessage(message string) {
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "error"), strings.Contains(lower, "failed"), strings.Contains(lower, "fatal"):
		_ = writer.eventLog.Error(eventIDLogError, message)
	case strings.Contains(lower, "warning"), strings.Contains(lower, "insecure"), strings.Contains(lower, "no-auth"):
		_ = writer.eventLog.Warning(eventIDLogWarning, message)
	default:
		_ = writer.eventLog.Info(eventIDLogInfo, message)
	}
}
