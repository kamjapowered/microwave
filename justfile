version := "0.1.0"
go := "go"
cache := justfile_directory() / ".cache/go-build"
dist := justfile_directory() / "dist"
prefix := env_var_or_default("PREFIX", home_directory() / ".local")

# ~/projects/go.work covers this tree and does not list this module, so a bare
# go command fails here. ci has no workspace, so joining one would make local
# and ci resolve differently.
export GOWORK := "off"

default:
	@just --list

# everything ci runs, so a green local run means a green ci run
ci: fmt vet tidy race link

fmt:
	@test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }

vet:
	GOCACHE={{cache}} {{go}} vet ./...

tidy:
	{{go}} mod tidy -diff

test:
	GOCACHE={{cache}} {{go}} test ./...

race:
	GOCACHE={{cache}} {{go}} test -race ./...

# link every test binary with inlining off, see golang/go#80976
link:
	GOCACHE={{cache}} {{go}} test -gcflags="all=-N -l" -run='^$' ./...

# there are no prebuilt binaries. microwave generates go code and is called
# from go:generate, so everyone who can use it already has a go toolchain:
# `go install kamjapowered.com/microwave@latest` is the install path, and the
# tag is the release. this builds a local binary for development only
build:
	mkdir -p {{dist}}
	{{go}} build -buildvcs=false -o {{dist}}/microwave .

install:
	mkdir -p {{prefix}}/bin
	{{go}} build -buildvcs=false -o {{prefix}}/bin/microwave .

clean:
	rm -rf {{dist}}

release: ci
	git diff --check
	git status --short
	git add .
	git commit -m "v{{version}}"
	git tag -a v{{version}} -m "v{{version}}"
	git push --follow-tags
