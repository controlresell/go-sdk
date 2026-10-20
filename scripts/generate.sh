#!/usr/bin/env bash
# Regenerates the client from the OpenAPI document of the ControlResell public API.
#
#   scripts/generate.sh [spec]   # a file or a URL; default: https://api.controlresell.com/api/public/openapi.json
#
# Models keep unknown fields (disallowAdditionalPropertiesIfNotPresent=false): v1 only ever adds fields.
# Rewrites the generated files of the root package (listed in .generated-files) and .spec-sha256. The document
# itself is not committed.
set -euo pipefail

GENERATOR_VERSION=7.26.0
SPEC="${1:-https://api.controlresell.com/api/public/openapi.json}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

JAR="$ROOT/.generator/openapi-generator-cli-$GENERATOR_VERSION.jar"
if [ ! -f "$JAR" ]; then
  mkdir -p "$ROOT/.generator"
  curl -fsSL -o "$JAR" \
    "https://repo1.maven.org/maven2/org/openapitools/openapi-generator-cli/$GENERATOR_VERSION/openapi-generator-cli-$GENERATOR_VERSION.jar"
fi

case "$SPEC" in
  http://*|https://*) curl -fsSL -o "$WORK/openapi.json" "$SPEC" ;;
  *) cp "$SPEC" "$WORK/openapi.json" ;;
esac

java -jar "$JAR" generate \
  -i "$WORK/openapi.json" \
  -g go \
  -o "$WORK/out" \
  --global-property=apiTests=false,modelTests=false,apiDocs=false,modelDocs=false \
  --additional-properties=packageName=controlresell,isGoSubmodule=true,withGoMod=false,hideGenerationTimestamp=true,disallowAdditionalPropertiesIfNotPresent=false \
  > "$WORK/generate.log"

if [ -f "$ROOT/.generated-files" ]; then
  while read -r f; do rm -f "$ROOT/$f"; done < "$ROOT/.generated-files"
fi
(cd "$WORK/out" && ls *.go | sort) > "$ROOT/.generated-files"
cp "$WORK"/out/*.go "$ROOT/"

shasum -a 256 "$WORK/openapi.json" | cut -d' ' -f1 > "$ROOT/.spec-sha256"
echo "Generated from $SPEC (sha256 $(cat "$ROOT/.spec-sha256"))"
