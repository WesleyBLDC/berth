package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"berth/spike/go/chrome"
	"berth/spike/go/gen/spike/v1/spikev1connect"

	spikev1 "berth/spike/go/gen/spike/v1"
)

const animatedPage = `<!doctype html><body style="margin:0;background:#111">
<canvas id=c width=1440 height=900></canvas><script>
const c=document.getElementById('c'),x=c.getContext('2d');let f=0;
function d(){f++;x.fillStyle='#111';x.fillRect(0,0,1440,900);
for(let i=0;i<40;i++){x.fillStyle='hsl('+((i*9+f)%360)+',70%,60%)';
x.beginPath();x.arc(720+Math.cos(f/30+i)*(200+i*8),450+Math.sin(f/25+i)*(150+i*5),30,0,7);x.fill();}
x.fillStyle='#fff';x.font='48px -apple-system';x.fillText('Berth live view frame '+f,40,80);requestAnimationFrame(d);}d();
</script>`

type screencaster struct {
	browser *chrome.Browser
	dir     string
	seq     atomic.Int32

	mu       sync.Mutex
	watchers map[chan *spikev1.WatchScreenResponse]struct{}
}

func startScreencaster(ctx context.Context) (*screencaster, error) {
	dir, err := os.MkdirTemp("", "berth-spike-screen-")
	if err != nil {
		return nil, err
	}
	b, err := chrome.Launch(ctx, chrome.Options{UserDataDir: dir, Headless: true})
	if err != nil {
		return nil, err
	}
	s := &screencaster{browser: b, dir: dir, watchers: map[chan *spikev1.WatchScreenResponse]struct{}{}}

	res, err := b.Call(ctx, "", "Target.createTarget", map[string]any{"url": "data:text/html," + url.PathEscape(animatedPage)})
	if err != nil {
		return nil, err
	}
	var target struct{ TargetID string }
	_ = json.Unmarshal(res, &target)
	res, err = b.Call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true})
	if err != nil {
		return nil, err
	}
	var attached struct{ SessionID string }
	_ = json.Unmarshal(res, &attached)
	sid := attached.SessionID
	for _, step := range []struct {
		method string
		params map[string]any
	}{
		{"Page.enable", nil},
		{"Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 900, "deviceScaleFactor": 1, "mobile": false}},
		{"Page.startScreencast", map[string]any{"format": "jpeg", "quality": 70, "maxWidth": 1440, "maxHeight": 900, "everyNthFrame": 1}},
	} {
		if _, err := b.Call(ctx, sid, step.method, step.params); err != nil {
			return nil, err
		}
	}
	go s.pump(sid)
	slog.Info("screencast started", "chrome_pid", b.PID())
	return s, nil
}

func (s *screencaster) pump(sid string) {
	for raw := range s.browser.Incoming {
		var m chrome.Message
		if json.Unmarshal(raw, &m) != nil || m.Method != "Page.screencastFrame" {
			continue
		}
		var frame struct {
			Data      string `json:"data"`
			SessionID int    `json:"sessionId"`
		}
		_ = json.Unmarshal(m.Params, &frame)
		// Chrome sends the next frame only after an ack, so acking first keeps frames flowing.
		go s.browser.Call(context.Background(), sid, "Page.screencastFrameAck", map[string]any{"sessionId": frame.SessionID})
		jpeg, err := base64.StdEncoding.DecodeString(frame.Data)
		if err != nil {
			continue
		}
		msg := &spikev1.WatchScreenResponse{Seq: s.seq.Add(1), Jpeg: jpeg, SentUnixMicros: time.Now().UnixMicro()}
		s.mu.Lock()
		for ch := range s.watchers {
			// Drop the stale frame instead of queueing: live view must never lag.
			select {
			case <-ch:
			default:
			}
			ch <- msg
		}
		s.mu.Unlock()
	}
}

func (s *screencaster) watch(ctx context.Context, stream spikev1connect.SpikeServiceWatchScreenServerStream) error {
	ch := make(chan *spikev1.WatchScreenResponse, 1)
	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-ch:
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func (s *screencaster) close() {
	s.browser.Close()
	_ = os.RemoveAll(s.dir)
}
