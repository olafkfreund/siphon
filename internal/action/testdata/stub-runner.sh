#!/bin/sh
if [ "$1" = --large ]; then head -c 1100000 /dev/zero; exit 0; fi
printf '{"argv":['
separator=
config=
previous=
for arg do
  if [ "$previous" = --mcp-config ]; then config=$arg; fi
  previous=$arg
  escaped=$(printf '%s' "$arg" | sed 's/\\/\\\\/g; s/"/\\"/g')
  printf '%s"%s"' "$separator" "$escaped"
  separator=,
done
stdin=$(cat)
escaped=$(printf '%s' "$stdin" | sed 's/\\/\\\\/g; s/"/\\"/g')
printf '],"stdin":"%s"}\n' "$escaped"
if [ "$1" = --echo-config ]; then cat "$config" >&2; fi
if [ "$1" = --fail ]; then exit 7; fi
