# Sample Files

`video.h264` and `audio.aac` are **5-second** clips extracted from the
[Twitch Sync Footage V1](https://archive.org/details/twitch-sync-footage-v1/Sync-Footage-V1-H264.mp4)
video available on the Internet Archive, used to run the demo out of the box.

To use your own source, generate the files with the commands below (adjust `-t` as needed).

---

## Generate H.264 with B-frames

`-b_strategy 0` and `-x264-params "bframes=2:b-adapt=0"` disable libx264's adaptive
B-frame decision so B-frames are actually emitted; without them the encoder often
produces few or no B-frames depending on content.

```bash
ffmpeg -i input.mp4 \
  -t 5 \
  -an \
  -c:v libx264 \
  -bf 2 \
  -b_strategy 0 \
  -x264-params "bframes=2:b-adapt=0:ref=3" \
  -g 50 \
  -r 25 \
  -preset medium \
  -crf 23 \
  sample/video.h264
```

## Extract AAC audio

```bash
ffmpeg -i input.mp4 \
  -t 5 \
  -vn \
  -c:a aac \
  -b:a 128k \
  sample/audio.aac
```

---

## Verify B-frames are present

```bash
ffprobe -v error -show_frames -select_streams v sample/video.h264 \
  | grep pict_type | sort | uniq -c
```

Expected output (B-frames confirmed):

```
  80 pict_type=B
   3 pict_type=I
  42 pict_type=P
```

If `pict_type=B` is missing, the video has no B-frames — check that your
encoder supports B-frame encoding and that `-b_strategy 0` is passed.
