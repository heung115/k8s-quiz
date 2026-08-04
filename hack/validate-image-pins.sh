#!/bin/sh
set -eu

fail=0

python3 problems/hack/validate_images.py --root problems

check_ref() {
  file=$1
  ref=$2
  if ! printf '%s\n' "$ref" | grep -Eq '@sha256:[0-9a-f]{64}$'; then
    echo "MUTABLE IMAGE: $file: $ref" >&2
    fail=1
  fi
}

for file in problems/*/setup.sh problems/*/verify.sh; do
  [ -f "$file" ] || continue
  refs=$(sed -nE "s/^[[:space:]]*image:[[:space:]]*[\"']?([^[:space:]\"'#]+).*/\1/p" "$file")
  for ref in $refs; do
    check_ref "$file" "$ref"
  done
  refs=$(sed -nE "s/.*--image(=|[[:space:]])[\"']?([^[:space:]\"'\\\\]+).*/\2/p" "$file")
  for ref in $refs; do
    check_ref "$file" "$ref"
  done
done

refs=$(sed -nE 's/^FROM[[:space:]]+([^[:space:]]+).*/\1/p' docker/k3s-base/Dockerfile)
for ref in $refs; do
  check_ref docker/k3s-base/Dockerfile "$ref"
done

[ "$fail" -eq 0 ]
echo "runtime image references are digest-pinned"
