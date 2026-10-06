#!/bin/sh
printf '['
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
printf ']\n'
if [ "$1" = --echo-config ]; then cat "$config" >&2; fi
