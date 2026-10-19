package main

import (
	"context"
	"os"
	"strings"
)

// activeStream is a cancellable background stream (log follow or terminal run)
// owned by one connection, so disconnecting the connection tears them down.
type activeStream struct {
	connectionID string
	cancel       context.CancelFunc
	terminal     *os.File
}

func (p *plugin) registerStream(id, connectionID string, cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.streams[id] = &activeStream{connectionID: connectionID, cancel: cancel}
}

func (p *plugin) unregisterStream(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.streams, id)
}

func (p *plugin) cancelStream(id string) bool {
	p.mu.Lock()
	stream, ok := p.streams[id]
	if ok {
		delete(p.streams, id)
	}
	p.mu.Unlock()
	if ok && stream.cancel != nil {
		stream.cancel()
	}
	return ok
}

func (p *plugin) cancelStreamsFor(connectionID string) {
	p.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(p.streams))
	for id, stream := range p.streams {
		if stream.connectionID == connectionID {
			cancels = append(cancels, stream.cancel)
			delete(p.streams, id)
		}
	}
	p.mu.Unlock()
	for _, cancel := range cancels {
		if cancel != nil {
			cancel()
		}
	}
}

// stripANSI removes terminal colour/cursor sequences. Pod logs and kubectl
// output are full of them, and they render as garbage in a plain <pre>.
func stripANSI(value string) string {
	if !strings.ContainsRune(value, 0x1b) {
		return value
	}
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != 0x1b {
			out.WriteByte(value[i])
			continue
		}
		i++
		if i >= len(value) {
			break
		}
		switch value[i] {
		case '[': // CSI: ESC [ params letter
			i++
			for i < len(value) && !(value[i] >= 0x40 && value[i] <= 0x7e) {
				i++
			}
		case ']': // OSC: ESC ] ... BEL or ESC \
			i++
			for i < len(value) && value[i] != 0x07 && value[i] != 0x1b {
				i++
			}
			if i < len(value) && value[i] == 0x1b {
				i++
			}
		default:
			// two-byte sequence, already skipped
		}
	}
	return out.String()
}
