# Demo video pipeline

Regenerates `docs/media/trove-demo.mp4`: real Trove sessions recorded in a
pseudo-terminal, rendered as frames, voiced with [Kokoro](https://github.com/thewh1teagle/kokoro-onnx)
text-to-speech and assembled with ffmpeg.

Nothing touches a real account: `setup_demo.sh` builds a throwaway world
(local source repositories, a work checkout that pushes to a local bare repo,
a Trove config) and `fake_github.py` serves a small fictional GitHub API
("acme" organization) that the real GitHub driver talks to.

| File | Role |
|------|------|
| `make_video.py` | scenes, pty recording, pyte terminal emulation, Pillow rendering, Kokoro narration, ffmpeg assembly |
| `fake_github.py` | fake GitHub REST API with demo data |
| `setup_demo.sh` | demo repositories, work checkout and config |

## Requirements

- macOS (fonts: Menlo, SF, Apple Symbols), git, Go, ffmpeg with libx264
- Python 3.13 with the packages in `requirements.txt`
- Kokoro model files (downloaded once, ~206 MB) from the
  `kokoro-onnx` release `model-files-v1.0`: `kokoro-v1.0.fp16.onnx` and
  `voices-v1.0.bin`

## Run

```sh
cd scripts/demo-video
python3 -m venv venv && ./venv/bin/pip install -r requirements.txt
mkdir -p models && cd models
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.fp16.onnx
curl -LO https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin
cd ..
go build -o trove ../../cmd/trove
./venv/bin/python make_video.py ../../docs/media/trove-demo.mp4
```

Scenes, captions, key presses and narration live in `SCENES` at the top of
`make_video.py`. Keys are `(delay_seconds, bytes)` pairs sent to the running
command; each key is written separately because Bubble Tea groups a burst of
characters into one key event.

Notes:

- espeak-ng (used by Kokoro for pronunciation) ignores data paths longer than
  about 160 characters, so the script points `ESPEAK_DATA_PATH` at a short
  relative path.
- Box-drawing characters are drawn as vector lines so borders join across the
  taller line height; glyphs Menlo lacks fall back to Apple Symbols.
