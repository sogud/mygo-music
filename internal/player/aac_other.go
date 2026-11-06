//go:build !darwin

package player

import (
	"errors"

	"github.com/egoist/mygo/audio"
)

func newAACDecoder(*download, int64, bool) (audio.Decoder, func(), error) {
	return nil, nil, errors.New("aac: AAC/M4A playback is currently supported on macOS only")
}
