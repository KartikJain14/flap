#!/usr/bin/env bash
# Convert the SVG frames written by "go test ./internal/tui -run TestShots"
# into the PNG screenshots used by the README. Needs ImageMagick.
set -euo pipefail
cd "$(dirname "$0")"
render() { magick -density "${3:-72}" "svg/$1.svg" "$2.png"; }
render welcome-100x30 welcome
render play-100x30 play
render stage2-100x30 golden-hour
render stage3-100x30 nightfall
render stage4-100x30 aurora
render stage5-100x30 daybreak
render over-80x24 game-over 80
render board-100x30 leaderboard
render help-100x30 help
render welcome-50x18 compact 80
render toosmall-40x12 too-small 80
render play-100x30-ascii ascii
render play-100x30-16 sixteen-colors
render play-220x45-letterbox ultrawide 48
echo "rendered $(ls -1 ./*.png | wc -l) screenshots"
