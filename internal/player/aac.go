package player

import (
	"errors"
	"sync"

	"github.com/egoist/mygo/audio"
)

// aacStream uses the system decoder for ADTS AAC and AAC in an MPEG-4
// container. It shares the download with the spectrum and waveform readers.
type aacStream struct {
	audio.Decoder
	dl      *download
	spec    *spectrum
	pos     int64
	drop    func()
	dispose func()
	mu      sync.Mutex
	once    sync.Once
	closed  bool
}

func openAACStream(dl *download, offset int64, container bool) (*aacStream, error) {
	dec, dispose, err := newAACDecoder(dl, offset, container)
	if err != nil {
		return nil, err
	}
	return &aacStream{Decoder: dec, dl: dl, spec: newSpectrum(dec.SampleRate(), dec.Len()), drop: dl.Close, dispose: dispose}, nil
}

func (s *aacStream) Read(dst []float32) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("aac: stream closed")
	}
	n, err := s.Decoder.Read(dst)
	s.spec.feed(s.pos, dst[:n])
	s.pos += int64(n / 2)
	return n, err
}

func (s *aacStream) SetPosition(frame int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("aac: stream closed")
	}
	frame = max(0, frame)
	if length := s.Len(); length >= 0 {
		frame = min(frame, length)
	}
	if err := s.Decoder.SetPosition(frame); err != nil {
		return err
	}
	s.pos = frame
	return nil
}

func (s *aacStream) Close() error {
	s.once.Do(func() {
		// Cancel a pending download read before waiting for the decoder.
		s.drop()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		s.dispose()
	})
	return nil
}

func (s *aacStream) source() (*download, *spectrum) { return s.dl, s.spec }
