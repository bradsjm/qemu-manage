package supervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"time"
)

type controlServer struct {
	cancel   context.CancelFunc
	listener *net.UnixListener
	done     <-chan struct{}
}

func (s *Service) startControlServer(listener *net.UnixListener, id string, run *supervisedRun) *controlServer {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Serve the control socket in the background so startup can return while the
	// listener continues accepting authenticated requests.
	go func() {
		defer close(done)
		s.serve(ctx, listener, id, run)
	}()
	return &controlServer{cancel: cancel, listener: listener, done: done}
}

func (c *controlServer) stop(run *supervisedRun) {
	if c == nil {
		return
	}
	if c.cancel != nil {
		c.cancel()
	}
	if c.listener != nil {
		_ = c.listener.Close()
	}
	if c.done != nil {
		<-c.done
	}
	if run != nil {
		run.closeNonStopConnections()
		run.handlers.Wait()
	}
}

func (s *Service) serve(ctx context.Context, listener *net.UnixListener, id string, run *supervisedRun) {
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		run.trackConnection(connection)
		go func() {
			defer run.untrackConnection(connection)
			s.handleConnection(connection, id, run)
		}()
	}
}

func (s *Service) handleConnection(connection *net.UnixConn, id string, run *supervisedRun) {
	defer connection.Close()
	// Authenticate the peer before decoding or dispatching any control command.
	uid, err := peerUID(connection)
	if err != nil || uid != uint32(os.Getuid()) {
		_ = EncodeResponse(connection, failure(id, ErrorUnauthorized, "control connection is not authorized"))
		return
	}
	request, err := DecodeRequest(connection)
	if err != nil {
		_ = EncodeResponse(connection, failure(id, ErrorInvalidRequest, err.Error()))
		return
	}
	if request.ID != id {
		_ = EncodeResponse(connection, failure(id, ErrorInvalidRequest, "request ID does not match running VM"))
		return
	}
	if request.Command == CommandStop {
		run.markStopConnection(connection)
	}
	// Dispatch the authenticated request against the current supervised run.
	switch request.Command {
	case CommandStatus:
		status, err := run.currentStatus(context.Background())
		if err != nil {
			_ = EncodeResponse(connection, failure(id, ErrorInternal, err.Error()))
			return
		}
		_ = EncodeResponse(connection, &Response{Version: ProtocolVersion, ID: id, OK: true, Status: &status})
	case CommandStop:
		timeout := run.defaultTimeout
		if request.TimeoutSeconds != nil {
			timeout = time.Duration(*request.TimeoutSeconds) * time.Second
		}
		err := run.stopWithProgress(context.Background(), request.Force, timeout, func(stage StopProgress) {
			progress := stage
			_ = EncodeResponse(connection, &Response{Version: ProtocolVersion, ID: id, OK: true, Progress: &progress})
		})
		if err != nil {
			code := ErrorInternal
			if errors.Is(err, errShutdownTimeout) {
				code = ErrorShutdownTimeout
			}
			_ = EncodeResponse(connection, failure(id, code, err.Error()))
			return
		}
		_ = EncodeResponse(connection, &Response{Version: ProtocolVersion, ID: id, OK: true})
	}
}

// failure builds a protocol error response
func failure(id string, code ErrorCode, message string) *Response {
	return &Response{Version: ProtocolVersion, ID: id, OK: false, Error: &ProtocolError{Code: code, Message: message}}
}
