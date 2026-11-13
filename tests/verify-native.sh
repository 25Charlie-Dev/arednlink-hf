#!/bin/sh
set -eu
# Run only inside an offline build container with /src read-only and /out writable.
# Fail on unformatted Go before any copy or rewrite.
unformatted=$(cd /src && gofmt -l cmd internal)
if [ -n "$unformatted" ]; then
 printf 'gofmt: unformatted files:\n%s\n' "$unformatted" >&2
 exit 1
fi
mkdir -p /out/formatted
cp -R /src/. /out/formatted
chmod -R u+w /out/formatted
cd /out/formatted
find internal cmd -name '*.go' -exec gofmt -w '{}' +
find internal cmd -name '*.go' -print0 | xargs -0 tar -cf /out/formatted-go.tar
go version
for profile in '' kiss js8call 'kiss js8call'; do
 printf 'PROFILE %s\n' "$profile"
 go test -race -tags "$profile" ./...
 go vet -tags "$profile" ./...
done
if go list -deps ./cmd/arednlink-hf | grep -E '^(net/http|html/template|arednlink-hf/internal/web)$'; then
 echo 'forbidden daemon dependency' >&2
 exit 1
fi
for profile in '' kiss js8call 'kiss js8call'; do
 label=$(printf '%s' "${profile:-none}" | tr ' ' '-')
 CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -tags "$profile" -trimpath -buildvcs=false -ldflags '-s -w -X main.version=native-ipc-candidate' -o "/out/arednlink-hf-$label-armv7" ./cmd/arednlink-hf
 wc -c "/out/arednlink-hf-$label-armv7"
done

CGO_ENABLED=0 go build -tags 'kiss js8call' -trimpath -buildvcs=false -ldflags '-s -w -X main.version=native-ipc-candidate' -o /out/arednlink-hf-combined-amd64 ./cmd/arednlink-hf
for smoke in control receive config resource_guard; do
 python3 "tests/artifact_${smoke}_smoke.py" --binary /out/arednlink-hf-combined-amd64
done
