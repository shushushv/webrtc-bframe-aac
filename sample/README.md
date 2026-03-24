# Sample Files

`video.h264` and `audio.aac` are **5-second** clips extracted from the
[Twitch Sync Footage V1](https://archive.org/details/twitch-sync-footage-v1/Sync-Footage-V1-H264.mp4)
video available on the Internet Archive, used to run the demo out of the box.

To use your own source, generate the files with the commands below (adjust `-t` as needed).

---

## Generate H.264 with B-frames

```bash
ffmpeg -i input.mp4 \
  -t 5 \        # 5-second clip
  -an \
  -c:v libx264 \
  -bf 2 \       # max 2 consecutive B-frames
  -g 60 \       # keyframe interval
  -r 25 \       # output frame rate
  -preset fast \
  -crf 23 \
  sample/video.h264
```

## Extract AAC audio

```bash
ffmpeg -i input.mp4 \
  -t 5 \        # 5-second clip
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
 335 pict_type=B
   5 pict_type=I
 380 pict_type=P
```

If `pict_type=B` is missing, the video has no B-frames — increase `-bf` or check
that your encoder supports B-frame encoding.
