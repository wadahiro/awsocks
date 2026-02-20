package ssm

import (
	"context"
	"net"
	"time"

	"github.com/wadahiro/awsocks/internal/backend/ssm/datachannel"
)

// dataBridge manages the lifecycle of a net.Pipe and its transfer goroutine.
// Once created, sshConn and dcConn are always valid until Close() is called,
// eliminating nil-check races.
type dataBridge struct {
	sshConn net.Conn // SSH client side of the pipe
	dcConn  net.Conn // DataChannel side of the pipe
	cancel  context.CancelFunc
	done    chan struct{} // closed when transfer goroutine exits (structured concurrency)
}

// newDataBridge creates a new dataBridge with a net.Pipe.
// The returned bridge owns both ends of the pipe and the cancel function.
func newDataBridge(parentCtx context.Context) (*dataBridge, context.Context) {
	ctx, cancel := context.WithCancel(parentCtx)
	sshConn, dcConn := net.Pipe()
	return &dataBridge{
		sshConn: sshConn,
		dcConn:  dcConn,
		cancel:  cancel,
		done:    make(chan struct{}),
	}, ctx
}

// Close closes both ends of the pipe and waits for the transfer goroutine to exit.
func (db *dataBridge) Close() {
	db.cancel()
	db.sshConn.Close()
	db.dcConn.Close()
	<-db.done // wait for transfer goroutine to finish
}

// startTransfer starts a goroutine that reads from dcConn and sends data
// to the DataChannel. The goroutine exits when dcConn is closed or the
// context is cancelled. Close() waits for this goroutine to finish.
func (db *dataBridge) startTransfer(ctx context.Context, dc *datachannel.DataChannel, logFn func(string, ...interface{})) {
	go func() {
		defer close(db.done)
		const chunkSize = 1024
		buf := make([]byte, chunkSize)

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			n, err := db.dcConn.Read(buf)
			if err != nil {
				// Pipe closed or context cancelled - normal during shutdown
				return
			}

			if err := dc.SendInputData(buf[:n]); err != nil {
				select {
				case <-ctx.Done():
					// Context cancelled, stop silently
				default:
					if logFn != nil {
						logFn("transferDataToSSM error: %v", err)
					}
				}
				return
			}

			// Rate limiting like the official plugin (1ms between sends)
			time.Sleep(time.Millisecond)
		}
	}()
}
