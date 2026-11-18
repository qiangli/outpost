package supervisor

import (
	"github.com/qiangli/yoke/pkg/secrets"
	"io"
	"os"
	"time"
)

// Pass a real descriptor to the child so os/exec waits only for that child,
// retaining the existing graceful-stop contract. This supervisor owns the
// reader and redacts before writing the log. A descendant retaining stdout
// after its parent exits must not keep the supervisor waiting indefinitely.
type childLogOutput struct {
	read, write *os.File
	done        chan struct{}
}

func newChildLogOutput(dst io.Writer) (*childLogOutput, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	output := &childLogOutput{read: read, write: write, done: make(chan struct{})}
	go func() {
		defer read.Close()
		writer := secrets.NewHostRedactingWriter(dst)
		_, _ = io.Copy(writer, read)
		_ = writer.Close()
		close(output.done)
	}()
	return output, nil
}

func (output *childLogOutput) close() {
	_ = output.write.Close()
	// Normal child exit closes its descriptor: drain all buffered output to EOF.
	// Only a descendant holding the pipe beyond child exit needs this bound.
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-output.done:
	case <-timer.C:
		_ = output.read.Close()
		<-output.done
	}
	_ = output.read.Close()
}
