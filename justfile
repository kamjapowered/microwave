default:
    @just --list

build:
    mkdir -p "${DIST:-dist}"
    GOWORK="${GOWORK:-off}" CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 "${GO:-go}" build -buildvcs=false -o "${DIST:-dist}/${BINARY:-microwave}_darwin_amd64" .
    GOWORK="${GOWORK:-off}" CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 "${GO:-go}" build -buildvcs=false -o "${DIST:-dist}/${BINARY:-microwave}_darwin_arm64" .
    GOWORK="${GOWORK:-off}" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "${GO:-go}" build -buildvcs=false -o "${DIST:-dist}/${BINARY:-microwave}_linux_amd64" .
    GOWORK="${GOWORK:-off}" CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "${GO:-go}" build -buildvcs=false -o "${DIST:-dist}/${BINARY:-microwave}_linux_arm64" .

build-local:
    mkdir -p "${DIST:-dist}"
    GOWORK="${GOWORK:-off}" "${GO:-go}" build -buildvcs=false -o "${DIST:-dist}/${BINARY:-microwave}" .

test:
    GOWORK="${GOWORK:-off}" "${GO:-go}" test ./...

vet:
    GOWORK="${GOWORK:-off}" "${GO:-go}" vet ./...

install:
    mkdir -p "${PREFIX:-$HOME/.local}/bin"
    GOWORK="${GOWORK:-off}" "${GO:-go}" build -buildvcs=false -o "${PREFIX:-$HOME/.local}/bin/${BINARY:-microwave}" .

clean:
    rm -rf "${DIST:-dist}"
