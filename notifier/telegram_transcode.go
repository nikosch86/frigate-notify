package notifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/0x2142/frigate-notify/models"
)

// Defaults used when a profile leaves the matching clip_transcode_* setting
// unset (<= 0).
const (
	clipTranscodeDefaultHeight  = 720
	clipTranscodeDefaultBitrate = 1000 // kbit/s
	clipTranscodeDefaultTimeout = 120  // seconds
)

// ffmpegPath is the ffmpeg binary used to transcode clips. It is a package-level
// seam so tests can substitute a stub.
var ffmpegPath = "ffmpeg"

// transcodeMu allows one transcode at a time, so a burst of alerts does not run
// several encoders at once.
var transcodeMu sync.Mutex

func intOrDefault(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// ffmpegTranscodeArgs builds the ffmpeg arguments that re-encode in to out as
// H.264/AAC, scaled down (never up) to the profile's height and capped at its
// video bitrate. With vaapi set, decoding, scaling & encoding run on the
// profile's VAAPI device instead of in software.
func ffmpegTranscodeArgs(in, out string, profile models.Telegram, vaapi bool) []string {
	// Encoders need even dimensions
	height := fmt.Sprintf("'2*trunc(min(%d,ih)/2)'", intOrDefault(profile.ClipTranscodeHeight, clipTranscodeDefaultHeight))
	bitrate := intOrDefault(profile.ClipTranscodeBitrate, clipTranscodeDefaultBitrate)

	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}
	var video []string
	if vaapi {
		args = append(args,
			"-init_hw_device", "vaapi=va:"+profile.ClipTranscodeVAAPIDevice,
			"-hwaccel", "vaapi", "-hwaccel_device", "va", "-hwaccel_output_format", "vaapi",
			"-filter_hw_device", "va")
		// format & hwupload cover inputs the GPU cannot decode, which reach the
		// filter as software frames
		video = []string{
			"-vf", "format=nv12|vaapi,hwupload,scale_vaapi=w=-2:h=" + height,
			"-c:v", "h264_vaapi", "-b:v", fmt.Sprintf("%dk", bitrate)}
	} else {
		video = []string{
			"-vf", "scale=-2:" + height,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p"}
	}

	args = append(args, "-i", in, "-map", "0:v:0", "-map", "0:a:0?")
	args = append(args, video...)
	return append(args,
		"-maxrate", fmt.Sprintf("%dk", bitrate), "-bufsize", fmt.Sprintf("%dk", 2*bitrate),
		"-c:a", "aac", "-b:a", "64k",
		"-movflags", "+faststart",
		out)
}

// runFFmpeg runs ffmpeg and returns its error output on failure.
func runFFmpeg(ctx context.Context, args []string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg timed out: %w", ctx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 1000 {
			detail = detail[len(detail)-1000:]
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, detail)
	}
	return nil
}

// transcodeClip re-encodes a clip with ffmpeg so it is lighter to download,
// returning the result and the video encoder that produced it. If the profile
// names a VAAPI device the GPU is tried first, with software as the fallback.
// The profile's timeout covers the whole pass.
func transcodeClip(clip []byte, profile models.Telegram) ([]byte, string, error) {
	dir, err := os.MkdirTemp("", "frigate-notify-clip-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)

	in, out := filepath.Join(dir, "in.mp4"), filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(in, clip, 0o600); err != nil {
		return nil, "", err
	}

	transcodeMu.Lock()
	defer transcodeMu.Unlock()

	timeout := time.Duration(intOrDefault(profile.ClipTranscodeTimeout, clipTranscodeDefaultTimeout)) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	encoder := "libx264"
	if device := profile.ClipTranscodeVAAPIDevice; device != "" {
		if err := runFFmpeg(ctx, ffmpegTranscodeArgs(in, out, profile, true)); err != nil {
			log.Warn().
				Err(err).
				Str("vaapi_device", device).
				Msg("Could not transcode clip with VAAPI, retrying in software")
		} else {
			encoder = "h264_vaapi"
		}
	}
	if encoder == "libx264" {
		if err := runFFmpeg(ctx, ffmpegTranscodeArgs(in, out, profile, false)); err != nil {
			return nil, "", err
		}
	}

	transcoded, err := os.ReadFile(out)
	if err != nil {
		return nil, "", err
	}
	if len(transcoded) == 0 {
		return nil, "", errors.New("ffmpeg produced an empty clip")
	}
	return transcoded, encoder, nil
}

// transcodeTelegramClip runs the profile's ffmpeg pass over a downloaded clip
// and logs the outcome, returning whichever of the two clips is smaller. It
// returns nil when the pass fails, which callers treat like a missing clip and
// fall back to the snapshot.
func transcodeTelegramClip(clip io.Reader, event models.Event, profile models.Telegram, provider notifMeta) io.Reader {
	start := time.Now()
	source, err := io.ReadAll(clip)
	if err == nil {
		var transcoded []byte
		var encoder string
		if transcoded, encoder, err = transcodeClip(source, profile); err == nil {
			log.Info().
				Str("event_id", event.ID).
				Str("review_id", event.Extra.ReviewID).
				Str("provider", "Telegram").
				Int("provider_id", provider.index).
				Str("encoder", encoder).
				Int("clip_bytes", len(source)).
				Int("transcoded_bytes", len(transcoded)).
				Int64("elapsed_ms", time.Since(start).Milliseconds()).
				Msg("Clip transcoded")
			// A clip already below the target bitrate can come out larger
			if len(transcoded) >= len(source) {
				log.Info().
					Str("event_id", event.ID).
					Str("review_id", event.Extra.ReviewID).
					Str("provider", "Telegram").
					Int("provider_id", provider.index).
					Msg("Transcoded clip is not smaller, using original clip")
				return bytes.NewReader(source)
			}
			return bytes.NewReader(transcoded)
		}
	}
	log.Warn().
		Str("event_id", event.ID).
		Str("review_id", event.Extra.ReviewID).
		Str("provider", "Telegram").
		Int("provider_id", provider.index).
		Err(err).
		Msg("Could not transcode clip, falling back to snapshot")
	return nil
}
