package player

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/egoist/mygo/audio"
)

// AudioToolbox is loaded dynamically so the app remains buildable without cgo.
// Native callbacks keep integer IDs, not Go pointers. The two callback thunks
// are allocated once; opening songs does not consume more callback slots.
var aacFiles sync.Map
var aacNextID atomic.Uint64

var loadAAC = sync.OnceValues(func() (*aacAPI, error) {
	lib, err := purego.Dlopen("/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	a := &aacAPI{}
	purego.RegisterLibFunc(&a.open, lib, "AudioFileOpenWithCallbacks")
	purego.RegisterLibFunc(&a.close, lib, "AudioFileClose")
	purego.RegisterLibFunc(&a.wrap, lib, "ExtAudioFileWrapAudioFileID")
	purego.RegisterLibFunc(&a.dispose, lib, "ExtAudioFileDispose")
	purego.RegisterLibFunc(&a.get, lib, "ExtAudioFileGetProperty")
	purego.RegisterLibFunc(&a.set, lib, "ExtAudioFileSetProperty")
	purego.RegisterLibFunc(&a.read, lib, "ExtAudioFileRead")
	purego.RegisterLibFunc(&a.seek, lib, "ExtAudioFileSeek")
	a.readCallback = purego.NewCallback(aacReadAt)
	a.sizeCallback = purego.NewCallback(aacSize)
	return a, nil
})

type aacAPI struct {
	open                       func(uintptr, uintptr, uintptr, uintptr, uintptr, uint32, *uintptr) int32
	close                      func(uintptr) int32
	wrap                       func(uintptr, uint8, *uintptr) int32
	dispose                    func(uintptr) int32
	get                        func(uintptr, uint32, *uint32, unsafe.Pointer) int32
	set                        func(uintptr, uint32, uint32, unsafe.Pointer) int32
	read                       func(uintptr, *uint32, *aacBufferList) int32
	seek                       func(uintptr, int64) int32
	readCallback, sizeCallback uintptr
}

// AudioStreamBasicDescription and an interleaved, one-buffer AudioBufferList.
// Pointer alignment supplies the padding required by both 64-bit macOS ABIs.
type aacFormat struct {
	rate                                                                                    float64
	format, flags, bytesPerPacket, framesPerPacket, bytesPerFrame, channels, bits, reserved uint32
}
type aacBufferList struct {
	count    uint32
	pad      uint32
	channels uint32
	size     uint32
	data     *float32
}

type aacDecoder struct {
	api           *aacAPI
	dl            *download
	offset        int64
	id, file, ext uintptr
	rate          int
	frames        int64
}

func aacStatus(op string, status int32) error {
	if status == 0 {
		return nil
	}
	return fmt.Errorf("aac: %s: AudioToolbox status %d", op, status)
}

func newAACDecoder(dl *download, offset int64, container bool) (audio.Decoder, func(), error) {
	a, err := loadAAC()
	if err != nil {
		return nil, nil, err
	}
	d := &aacDecoder{api: a, dl: dl, offset: offset, id: uintptr(aacNextID.Add(1)), frames: -1}
	aacFiles.Store(d.id, d)
	ok := false
	defer func() {
		if !ok {
			d.close()
		}
	}()
	typ := uint32(0x61647473) // 'adts'
	if container {
		typ = 0x6d346166 // 'm4af'
	}
	if err = aacStatus("open", a.open(d.id, a.readCallback, 0, a.sizeCallback, 0, typ, &d.file)); err != nil {
		return nil, nil, err
	}
	if err = aacStatus("wrap", a.wrap(d.file, 0, &d.ext)); err != nil {
		return nil, nil, err
	}
	var source aacFormat
	size := uint32(unsafe.Sizeof(source))
	if err = aacStatus("format", a.get(d.ext, 0x66666d74, &size, unsafe.Pointer(&source))); err != nil {
		return nil, nil, err
	}
	if source.rate <= 0 || source.rate > 768000 || source.channels == 0 {
		return nil, nil, fmt.Errorf("aac: invalid audio format")
	}
	d.rate = int(source.rate)
	client := aacFormat{rate: source.rate, format: 0x6c70636d, flags: 1 | 8, bytesPerPacket: 8, framesPerPacket: 1, bytesPerFrame: 8, channels: 2, bits: 32}
	if err = aacStatus("PCM format", a.set(d.ext, 0x63666d74, uint32(unsafe.Sizeof(client)), unsafe.Pointer(&client))); err != nil {
		return nil, nil, err
	}
	size = 8
	if a.get(d.ext, 0x2366726d, &size, unsafe.Pointer(&d.frames)) != 0 {
		d.frames = -1
	}
	ok = true
	return d, d.close, nil
}

func aacReadAt(id uintptr, position int64, count uint32, buffer unsafe.Pointer, actual *uint32) int32 {
	*actual = 0
	v, ok := aacFiles.Load(id)
	if !ok || position < 0 {
		return -1
	}
	d := v.(*aacDecoder)
	if position > math.MaxInt64-d.offset-int64(count) {
		return -1
	}
	off := position + d.offset
	d.dl.waitFor(off + int64(count))
	d.dl.mu.Lock()
	defer d.dl.mu.Unlock()
	n, err := d.dl.readAt(unsafe.Slice((*byte)(buffer), int(count)), off)
	*actual = uint32(n)
	if err != nil && err != io.EOF {
		return -1
	}
	if n == 0 && d.dl.err != nil {
		return -1
	}
	return 0
}

func aacSize(id uintptr) int64 {
	v, ok := aacFiles.Load(id)
	if !ok {
		return 0
	}
	d := v.(*aacDecoder)
	d.dl.mu.Lock()
	for d.dl.size <= 0 && !d.dl.done {
		d.dl.cond.Wait()
	}
	size := d.dl.size
	if d.dl.done {
		size = d.dl.n
	}
	d.dl.mu.Unlock()
	return max(0, size-d.offset)
}

func (d *aacDecoder) SampleRate() int { return d.rate }
func (d *aacDecoder) Channels() int   { return 2 }
func (d *aacDecoder) Len() int64      { return d.frames }
func (d *aacDecoder) SetPosition(frame int64) error {
	return aacStatus("seek", d.api.seek(d.ext, frame))
}
func (d *aacDecoder) Read(dst []float32) (int, error) {
	frames := uint32(min(len(dst)/2, math.MaxUint32/8))
	if frames == 0 {
		return 0, nil
	}
	list := aacBufferList{count: 1, channels: 2, size: frames * 8, data: &dst[0]}
	status := d.api.read(d.ext, &frames, &list)
	runtime.KeepAlive(dst)
	if err := aacStatus("decode", status); err != nil {
		return 0, err
	}
	if frames == 0 {
		return 0, io.EOF
	}
	return int(frames) * 2, nil
}
func (d *aacDecoder) close() {
	if d.ext != 0 {
		d.api.dispose(d.ext)
	}
	if d.file != 0 {
		d.api.close(d.file)
	}
	aacFiles.Delete(d.id)
}
