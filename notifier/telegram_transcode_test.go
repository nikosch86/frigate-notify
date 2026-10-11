package notifier

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/0x2142/frigate-notify/config"
	"github.com/0x2142/frigate-notify/models"
	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

// stubFFmpeg points ffmpegPath at a shell script with the given body for the
// duration of the test. The script receives the real ffmpeg arguments, so the
// output file is always its last argument.
func stubFFmpeg(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ffmpeg stub is a shell script")
	}
	stub := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("could not write ffmpeg stub: %v", err)
	}
	orig := ffmpegPath
	t.Cleanup(func() { ffmpegPath = orig })
	ffmpegPath = stub
}

// stubWriteOutput is a stub body that writes "TRANSCODED" to the output file.
const stubWriteOutput = `for out; do :; done; printf TRANSCODED > "$out"`

func TestFFmpegTranscodeArgs(t *testing.T) {
	vaapi := models.Telegram{ClipTranscodeVAAPIDevice: "/dev/dri/renderD128"}

	tests := []struct {
		name    string
		profile models.Telegram
		vaapi   bool
		want    []string
	}{
		{"defaults when unset", models.Telegram{}, false, []string{"scale=-2:'2*trunc(min(720,ih)/2)'", "libx264", "1000k", "2000k"}},
		{"defaults when negative", models.Telegram{ClipTranscodeHeight: -1, ClipTranscodeBitrate: -1}, false, []string{"scale=-2:'2*trunc(min(720,ih)/2)'", "1000k", "2000k"}},
		{"custom values", models.Telegram{ClipTranscodeHeight: 480, ClipTranscodeBitrate: 500}, false, []string{"scale=-2:'2*trunc(min(480,ih)/2)'", "500k", "1000k"}},
		{"vaapi", vaapi, true, []string{"vaapi=va:/dev/dri/renderD128", "format=nv12|vaapi,hwupload,scale_vaapi=w=-2:h='2*trunc(min(720,ih)/2)'", "h264_vaapi", "1000k", "2000k"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			args := ffmpegTranscodeArgs("in.mp4", "out.mp4", tc.profile, tc.vaapi)
			for _, want := range tc.want {
				if !slices.Contains(args, want) {
					t.Errorf("expected argument %q in %v", want, args)
				}
			}
			if args[len(args)-1] != "out.mp4" {
				t.Errorf("expected output file as last argument, got %q", args[len(args)-1])
			}
		})
	}

	t.Run("software args do not touch the vaapi device", func(t *testing.T) {
		args := strings.Join(ffmpegTranscodeArgs("in.mp4", "out.mp4", vaapi, false), " ")
		if strings.Contains(args, "vaapi") {
			t.Errorf("expected no vaapi arguments in the software fallback, got %s", args)
		}
	})
}

func TestTranscodeClip(t *testing.T) {
	t.Run("returns ffmpeg output", func(t *testing.T) {
		stubFFmpeg(t, stubWriteOutput)
		got, encoder, err := transcodeClip([]byte("SOURCE"), models.Telegram{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "TRANSCODED" {
			t.Errorf("expected transcoded bytes, got %q", got)
		}
		if encoder != "libx264" {
			t.Errorf("expected software encoder, got %q", encoder)
		}
	})

	t.Run("passes the clip as input file", func(t *testing.T) {
		// Copy the file following -i to the output file
		stubFFmpeg(t, `for out; do :; done; while [ "$1" != "-i" ]; do shift; done; cp "$2" "$out"`)
		got, _, err := transcodeClip([]byte("SOURCE"), models.Telegram{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "SOURCE" {
			t.Errorf("expected ffmpeg to read the source clip, got %q", got)
		}
	})

	t.Run("reports ffmpeg failure with its error output", func(t *testing.T) {
		stubFFmpeg(t, `echo "moov atom not found" >&2; exit 1`)
		_, _, err := transcodeClip([]byte("SOURCE"), models.Telegram{})
		if err == nil || !strings.Contains(err.Error(), "moov atom not found") {
			t.Errorf("expected error carrying ffmpeg's stderr, got %v", err)
		}
	})

	t.Run("fails when ffmpeg writes no output", func(t *testing.T) {
		stubFFmpeg(t, `exit 0`)
		if _, _, err := transcodeClip([]byte("SOURCE"), models.Telegram{}); err == nil {
			t.Error("expected an error when no output was produced")
		}
	})

	t.Run("gives up after the timeout", func(t *testing.T) {
		stubFFmpeg(t, `exec sleep 30`)
		start := time.Now()
		_, _, err := transcodeClip([]byte("SOURCE"), models.Telegram{ClipTranscodeTimeout: 1})
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("expected a timeout error, got %v", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Errorf("expected ffmpeg to be killed at the timeout, took %s", elapsed)
		}
	})

	vaapi := models.Telegram{ClipTranscodeVAAPIDevice: "/dev/dri/renderD128"}

	t.Run("uses vaapi when a device is configured", func(t *testing.T) {
		stubFFmpeg(t, `case "$*" in *h264_vaapi*) ;; *) exit 1;; esac; `+stubWriteOutput)
		got, encoder, err := transcodeClip([]byte("SOURCE"), vaapi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "TRANSCODED" || encoder != "h264_vaapi" {
			t.Errorf("expected vaapi transcode, got %q from encoder %q", got, encoder)
		}
	})

	t.Run("falls back to software when vaapi fails", func(t *testing.T) {
		stubFFmpeg(t, `case "$*" in *h264_vaapi*) echo "Device creation failed" >&2; exit 1;; esac; `+stubWriteOutput)
		got, encoder, err := transcodeClip([]byte("SOURCE"), vaapi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "TRANSCODED" || encoder != "libx264" {
			t.Errorf("expected software fallback, got %q from encoder %q", got, encoder)
		}
	})
}

// TestTranscodeClipFFmpeg runs the real ffmpeg over a generated 1080p clip with
// audio and checks the result is a 720p H.264/AAC file that is smaller than the
// source. Skipped when ffmpeg is not installed; the VAAPI case is also skipped
// when the render device is not accessible.
func TestTranscodeClipFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	source := filepath.Join(t.TempDir(), "source.mp4")
	gen := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=15:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libx264", "-b:v", "6M", "-c:a", "aac",
		"-movflags", "frag_keyframe+empty_moov", source)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("could not generate source clip: %v: %s", err, out)
	}
	clip, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}

	const vaapiDevice = "/dev/dri/renderD128"
	tests := []struct {
		name    string
		profile models.Telegram
		encoder string
	}{
		{"software", models.Telegram{}, "libx264"},
		{"vaapi", models.Telegram{ClipTranscodeVAAPIDevice: vaapiDevice}, "h264_vaapi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.profile.ClipTranscodeVAAPIDevice != "" {
				device, err := os.OpenFile(vaapiDevice, os.O_RDWR, 0)
				if err != nil {
					t.Skipf("VAAPI device not accessible: %v", err)
				}
				device.Close()
			}

			got, encoder, err := transcodeClip(clip, tc.profile)
			if err != nil {
				t.Fatalf("transcode failed: %v", err)
			}
			if encoder != tc.encoder {
				t.Errorf("expected encoder %q, got %q", tc.encoder, encoder)
			}
			if len(got) >= len(clip) {
				t.Errorf("expected transcoded clip to be smaller: source %d bytes, transcoded %d bytes", len(clip), len(got))
			}

			// ffmpeg prints the stream details of its input before failing for
			// lack of an output file
			result := filepath.Join(t.TempDir(), "result.mp4")
			if err := os.WriteFile(result, got, 0o600); err != nil {
				t.Fatal(err)
			}
			info, _ := exec.Command("ffmpeg", "-hide_banner", "-i", result).CombinedOutput()
			for _, want := range []string{"Video: h264", "1280x720", "Audio: aac"} {
				if !strings.Contains(string(info), want) {
					t.Errorf("expected %q in transcoded clip details:\n%s", want, info)
				}
			}
		})
	}
}

// telegramCalls returns the endpoints of the recorded non-getMe requests.
func telegramCalls(reqs []capturedReq) []capturedReq {
	var calls []capturedReq
	for _, r := range reqs {
		if r.endpoint != "getMe" {
			calls = append(calls, r)
		}
	}
	return calls
}

// TestSendTelegramSnapshotThenClipTranscoded verifies that with clip_transcode
// enabled the snapshot is replaced by the transcoded clip, and that the size
// limit applies to the transcoded clip rather than the download.
func TestSendTelegramSnapshotThenClipTranscoded(t *testing.T) {
	stubFFmpeg(t, stubWriteOutput)

	// ~2 MB source clip with a 1 MB limit: only the transcoded clip fits
	clipData := bytes.Repeat([]byte("x"), 2*1024*1024)
	frigate := newMockFrigateClipServer(clipData)
	defer frigate.Close()

	tg, reqs, mu := newMockTelegram(t)
	defer tg.Close()

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", tg.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}

	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Alerts.General.MaxSnapRetry = 2

	profile := models.Telegram{ChatID: 1, ClipMaxSize: 1, ClipTranscode: true}
	status := &models.NotifierStatus{}
	event := models.Event{ID: "evt1", HasSnapshot: true, HasClip: true}
	provider := notifMeta{name: "telegram", index: 0}

	sendTelegramSnapshotThenClip(bot, profile, status, event, bytes.NewReader([]byte("FAKE-SNAPSHOT")), "caption", provider)

	mu.Lock()
	defer mu.Unlock()

	calls := telegramCalls(*reqs)
	if got := endpoints(calls); !slices.Equal(got, []string{"sendPhoto", "editMessageMedia"}) {
		t.Fatalf("expected sendPhoto then editMessageMedia, got %v", got)
	}
	if !strings.Contains(calls[1].body, "TRANSCODED") {
		t.Error("expected editMessageMedia to upload the transcoded clip")
	}
	if strings.Contains(calls[1].body, "xxxxxxxx") {
		t.Error("expected the source clip not to be uploaded")
	}
}

// TestSendTelegramSnapshotThenClipTranscodeFailed verifies the snapshot is kept
// when ffmpeg fails.
func TestSendTelegramSnapshotThenClipTranscodeFailed(t *testing.T) {
	stubFFmpeg(t, `exit 1`)

	frigate := newMockFrigateClipServer([]byte("FAKE-CLIP-BYTES"))
	defer frigate.Close()

	tg, reqs, mu := newMockTelegram(t)
	defer tg.Close()

	bot, err := tgbotapi.NewBotAPIWithAPIEndpoint("token", tg.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("failed to create mock bot: %v", err)
	}

	config.ConfigData.Frigate.Server = frigate.URL
	config.ConfigData.Alerts.General.MaxSnapRetry = 2

	profile := models.Telegram{ChatID: 1, ClipTranscode: true}
	status := &models.NotifierStatus{}
	event := models.Event{ID: "evt1", HasSnapshot: true, HasClip: true}
	provider := notifMeta{name: "telegram", index: 0}

	sendTelegramSnapshotThenClip(bot, profile, status, event, bytes.NewReader([]byte("FAKE-SNAPSHOT")), "caption", provider)

	mu.Lock()
	defer mu.Unlock()

	if got := endpoints(telegramCalls(*reqs)); !slices.Equal(got, []string{"sendPhoto"}) {
		t.Errorf("expected only the snapshot to be sent, got %v", got)
	}
	if status.Sent != 1 || status.Failed != 0 {
		t.Errorf("expected status sent=1 failed=0, got sent=%d failed=%d", status.Sent, status.Failed)
	}
}

// TestSendTelegramClipTranscode covers the send_clip flow (no snapshot-first):
// the transcoded clip is sent as the video unless the original is smaller, and
// the snapshot is sent instead when ffmpeg fails.
func TestSendTelegramClipTranscode(t *testing.T) {
	tests := []struct {
		name     string
		stub     string
		endpoint string
		upload   string
	}{
		{"sends transcoded clip", stubWriteOutput, "sendVideo", "TRANSCODED"},
		{"sends original clip when the transcode is not smaller", `for out; do :; done; printf TRANSCODED-BUT-LARGER-THAN-THE-SOURCE > "$out"`, "sendVideo", "FAKE-CLIP-BYTES"},
		{"sends snapshot when ffmpeg fails", `exit 1`, "sendPhoto", "FAKE-SNAPSHOT"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stubFFmpeg(t, tc.stub)

			frigate := newMockFrigateClipServer([]byte("FAKE-CLIP-BYTES"))
			defer frigate.Close()

			tg, reqs, mu := newMockTelegram(t)
			defer tg.Close()

			origNewBot := newTelegramBot
			defer func() { newTelegramBot = origNewBot }()
			newTelegramBot = func(token string) (*tgbotapi.BotAPI, error) {
				return tgbotapi.NewBotAPIWithAPIEndpoint(token, tg.URL+"/bot%s/%s")
			}

			origTemplates := TemplateFiles
			defer func() { TemplateFiles = origTemplates }()
			TemplateFiles = os.DirFS("..")

			config.ConfigData.Frigate.Server = frigate.URL
			config.ConfigData.Alerts.General.MaxSnapRetry = 2

			profile := models.Telegram{ChatID: 1, SendClip: true, ClipTranscode: true}
			profile.Enabled = true
			config.ConfigData.Alerts.Telegram = []models.Telegram{profile}
			config.Internal.Status.Notifications.Telegram = make([]models.NotifierStatus, 1)

			event := models.Event{ID: "evt1", Camera: "front_door", HasSnapshot: true, HasClip: true}
			SendTelegramMessage(event, bytes.NewReader([]byte("FAKE-SNAPSHOT")), notifMeta{name: "telegram", index: 0})

			mu.Lock()
			defer mu.Unlock()

			calls := telegramCalls(*reqs)
			if got := endpoints(calls); !slices.Equal(got, []string{tc.endpoint}) {
				t.Fatalf("expected a single %s call, got %v", tc.endpoint, got)
			}
			if !strings.Contains(calls[0].body, tc.upload) {
				t.Errorf("expected %s to upload %q", tc.endpoint, tc.upload)
			}
		})
	}
}
