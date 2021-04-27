PROTOC ?= $(shell which protoc)
CWD    := $(shell pwd)
TOOLS  := $(CWD)/tools/bin
PACKAGES := go list ./... | grep -vE 'vendor|tools'
COVERED_PACKAGES := $(PACKAGES) | grep -vE 'mock|tests|checkpointspb'
PACKAGE_DIRECTORIES := $(PACKAGES) | sed 's/github.com\/pingcap\/br\/*//'
PACKAGE_FILES := $$(find . -name '*.go' -type f | grep -vE 'vendor|\.pb\.go|mock|tools|res_vfsdata')
CHECKER := awk '{ print } END { if (NR > 0) { exit 1 } }'

BR_PKG := github.com/pingcap/br

RELEASE_VERSION =
ifeq ($(RELEASE_VERSION),)
	RELEASE_VERSION := v5.0.0-master
	release_version_regex := ^v5\..*$$
	release_branch_regex := "^release-[0-9]\.[0-9].*$$|^HEAD$$|^.*/*tags/v[0-9]\.[0-9]\..*$$"
	ifneq ($(shell git rev-parse --abbrev-ref HEAD | egrep $(release_branch_regex)),)
		# If we are in release branch, try to use tag version.
		ifneq ($(shell git describe --tags --dirty | egrep $(release_version_regex)),)
			RELEASE_VERSION := $(shell git describe --tags --dirty)
		endif
	else ifneq ($(shell git status --porcelain),)
		# Add -dirty if the working tree is dirty for non release branch.
		RELEASE_VERSION := $(RELEASE_VERSION)-dirty
	endif
endif

LDFLAGS += -X "$(BR_PKG)/pkg/version/build.ReleaseVersion=$(RELEASE_VERSION)"
LDFLAGS += -X "$(BR_PKG)/pkg/version/build.BuildTS=$(shell date -u '+%Y-%m-%d %I:%M:%S')"
LDFLAGS += -X "$(BR_PKG)/pkg/version/build.GitHash=$(shell git rev-parse HEAD)"
LDFLAGS += -X "$(BR_PKG)/pkg/version/build.GitBranch=$(shell git rev-parse --abbrev-ref HEAD)"

LIGHTNING_BIN     := bin/tidb-lightning
LIGHTNING_CTL_BIN := bin/tidb-lightning-ctl
BR_BIN            := bin/br
VFSGENDEV_BIN     := tools/bin/vfsgendev
TEST_DIR          := /tmp/backup_restore_test

path_to_add := $(addsuffix /bin,$(subst :,/bin:,$(GOPATH)))
export PATH := $(path_to_add):$(PATH)

GOBUILD := CGO_ENABLED=1 GO111MODULE=on go build -trimpath -ldflags '$(LDFLAGS)'
GOTEST  := CGO_ENABLED=1 GO111MODULE=on go test -ldflags '$(LDFLAGS)'
PREPARE_MOD := cp go.mod1 go.mod && cp go.sum1 go.sum
FINISH_MOD := cp go.mod go.mod1 && cp go.sum go.sum1

RACEFLAG =
ifeq ("$(WITH_RACE)", "1")
	RACEFLAG = -race
	GOBUILD  = CGO_ENABLED=1 GO111MODULE=on $(GO) build -ldflags '$(LDFLAGS)'
endif

all: build check test

prepare:
	$(PREPARE_MOD)

finish-prepare:
	$(FINISH_MOD)

%_generated.go: %.rl
	ragel -Z -G2 -o tmp_parser.go $<
	@echo '// Code generated by ragel DO NOT EDIT.' | cat - tmp_parser.go | sed 's|//line |//.... |g' > $@
	@rm tmp_parser.go

data_parsers: tools pkg/lightning/mydump/parser_generated.go web
	PATH="$(GOPATH)/bin":"$(PATH)":"$(TOOLS)" protoc -I. -I"$(GOPATH)/src" pkg/lightning/checkpoints/checkpointspb/file_checkpoints.proto --gogofaster_out=.
	$(TOOLS)/vfsgendev -source='"github.com/pingcap/br/pkg/lightning/web".Res' && mv res_vfsdata.go pkg/lightning/web/

web:
	cd web && npm install && npm run build

build: br lightning lightning-ctl

br:
	$(PREPARE_MOD)
	$(GOBUILD) $(RACEFLAG) -o $(BR_BIN) cmd/br/*.go

lightning_for_web:
	$(PREPARE_MOD)
	$(GOBUILD) $(RACEFLAG) -tags dev -o $(LIGHTNING_BIN) cmd/tidb-lightning/main.go

lightning:
	$(PREPARE_MOD)
	$(GOBUILD) $(RACEFLAG) -o $(LIGHTNING_BIN) cmd/tidb-lightning/main.go

lightning-ctl:
	$(PREPARE_MOD)
	$(GOBUILD) $(RACEFLAG) -o $(LIGHTNING_CTL_BIN) cmd/tidb-lightning-ctl/main.go

build_for_integration_test:
	$(PREPARE_MOD)
	@make failpoint-enable
	($(GOTEST) -c -cover -covermode=count \
		-coverpkg=$(BR_PKG)/... \
		-o $(BR_BIN).test \
		github.com/pingcap/br/cmd/br && \
	$(GOTEST) -c -cover -covermode=count \
		-coverpkg=$(BR_PKG)/... \
		-o $(LIGHTNING_BIN).test \
		github.com/pingcap/br/cmd/tidb-lightning && \
	$(GOTEST) -c -cover -covermode=count \
		-coverpkg=$(BR_PKG)/... \
		-o $(LIGHTNING_CTL_BIN).test \
		github.com/pingcap/br/cmd/tidb-lightning-ctl && \
	$(GOBUILD) $(RACEFLAG) -o bin/locker tests/br_key_locked/*.go && \
	$(GOBUILD) $(RACEFLAG) -o bin/gc tests/br_z_gc_safepoint/*.go && \
	$(GOBUILD) $(RACEFLAG) -o bin/oauth tests/br_gcs/*.go && \
	$(GOBUILD) $(RACEFLAG) -o bin/rawkv tests/br_rawkv/*.go && \
	$(GOBUILD) $(RACEFLAG) -o bin/parquet_gen tests/lightning_checkpoint_parquet/*.go \
	) || (make failpoint-disable && exit 1)
	@make failpoint-disable

test: export ARGS=$$($(PACKAGES))
test:
	$(PREPARE_MOD)
	@make failpoint-enable
	$(GOTEST) $(RACEFLAG) -tags leak $(ARGS) || ( make failpoint-disable && exit 1 )
	@make failpoint-disable

testcover: tools
	mkdir -p "$(TEST_DIR)"
	$(PREPARE_MOD)
	@make failpoint-enable
	$(GOTEST) -cover -covermode=count -coverprofile="$(TEST_DIR)/cov.unit.out" \
		$$($(COVERED_PACKAGES)) || ( make failpoint-disable && exit 1 )
	@make failpoint-disable

integration_test: bins build build_for_integration_test
	tests/run.sh

compatibility_test_prepare:
	tests/run_compatible.sh prepare

compatibility_test: br
	tests/run_compatible.sh run

coverage: tools
	tools/bin/gocovmerge "$(TEST_DIR)"/cov.* | grep -vE ".*.pb.go|.*__failpoint_binding__.go" > "$(TEST_DIR)/all_cov.out"
ifeq ("$(JenkinsCI)", "1")
	tools/bin/goveralls -coverprofile=$(TEST_DIR)/all_cov.out -service=jenkins-ci -repotoken $(COVERALLS_TOKEN)
else
	go tool cover -html "$(TEST_DIR)/all_cov.out" -o "$(TEST_DIR)/all_cov.html"
	grep -F '<option' "$(TEST_DIR)/all_cov.html"
endif

bins:
	@which bin/tidb-server
	@which bin/tikv-server
	@which bin/pd-server
	@which bin/pd-ctl
	@which bin/go-ycsb
	@which bin/minio
	@which bin/tiflash
	@which bin/libtiflash_proxy.so
	@which bin/cdc
	@which bin/fake-gcs-server
	@which bin/tikv-importer
	if [ ! -d bin/flash_cluster_manager ]; then echo "flash_cluster_manager not exist"; exit 1; fi

tools:
	@echo "install tools..."
	@cd tools && make

check:
	@# Tidy first to avoid go.mod being affected by static and lint
	@make tidy
	@# Build tools for targets errdoc, static and lint
	@make tools errdoc static lint

static: export GO111MODULE=on
static: prepare tools
	@ # Not running vet and fmt through metalinter becauase it ends up looking at vendor
	tools/bin/gofumports -w -d -format-only -local $(BR_PKG) $(PACKAGE_FILES) 2>&1 | $(CHECKER)
	# TODO: go vet lightning packages too.
	tools/bin/govet --shadow $$($(PACKAGE_DIRECTORIES) | grep -v "lightning") 2>&1 | $(CHECKER)

	# TODO: lint lightning packages too.
	@# why some lints are disabled?
	@#   gochecknoglobals - disabled because we do use quite a lot of globals
	@#          goimports - executed above already, gofumports
	@#              gofmt - ditto
	@#                gci - ditto
	@#                wsl - too pedantic about the formatting
	@#             funlen - PENDING REFACTORING
	@#           gocognit - PENDING REFACTORING
	@#              godox - TODO
	@#              gomnd - too many magic numbers, and too pedantic (even 2*x got flagged...)
	@#        testpackage - several test packages still rely on private functions
	@#             nestif - PENDING REFACTORING
	@#           goerr113 - it mistaken pingcap/errors with standard errors
	@#                lll - pingcap/errors may need to write a long line
	@#       paralleltest - no need to run test parallel
	@#           nlreturn - no need to ensure a new line before continue or return
	@#   exhaustivestruct - Protobuf structs have hidden fields, like "XXX_NoUnkeyedLiteral"
	@#         exhaustive - no need to check exhaustiveness of enum switch statements
	@#              gosec - too many false positive
	@#          errorlint - pingcap/errors is incompatible with std errors.
	@#          wrapcheck - there are too many unwrapped errors in tidb-lightning
	CGO_ENABLED=0 tools/bin/golangci-lint run --enable-all --deadline 120s \
		--disable gochecknoglobals \
		--disable goimports \
		--disable gofmt \
		--disable gci \
		--disable wsl \
		--disable funlen \
		--disable gocognit \
		--disable godox \
		--disable gomnd \
		--disable testpackage \
		--disable nestif \
		--disable goerr113 \
		--disable lll \
		--disable paralleltest \
		--disable nlreturn \
		--disable exhaustivestruct \
		--disable exhaustive \
		--disable godot \
		--disable gosec \
		--disable errorlint \
		--disable wrapcheck \
		$(PACKAGE_DIRECTORIES)
	# pingcap/errors APIs are mixed with multiple patterns 'pkg/errors',
	# 'juju/errors' and 'pingcap/parser'. To avoid confusion and mistake,
	# we only allow a subset of APIs, that's "Normalize|Annotate|Trace|Cause|Find".
	# TODO: check lightning packages.
	@# TODO: allow more APIs when we need to support "workaound".
	grep -Rn --include="*.go" --exclude="*_test.go" -E "(\t| )errors\.[A-Z]" \
		$$($(PACKAGE_DIRECTORIES) | grep -vE "tests|lightning") | \
		grep -vE "Normalize|Annotate|Trace|Cause|RedactLogEnabled|Find" 2>&1 | $(CHECKER)
	# The package name of "github.com/pingcap/kvproto/pkg/backup" collides
	# "github.com/pingcap/br/pkg/backup", so we rename kvproto to backuppb.
	grep -Rn --include="*.go" -E '"github.com/pingcap/kvproto/pkg/backup"' \
		$$($(PACKAGE_DIRECTORIES)) | \
		grep -vE "backuppb" | $(CHECKER)

lint: prepare tools
	@echo "linting"
	# TODO: lint lightning packages.
	CGO_ENABLED=0 tools/bin/revive -formatter friendly -config revive.toml $$($(PACKAGES) | grep -v "lightning")

tidy:
	@echo "go mod tidy"
	$(PREPARE_MOD)
	GO111MODULE=on go mod tidy
	$(FINISH_MOD)
	cd tests && GO111MODULE=on go mod tidy
	git diff --exit-code go.mod1 go.sum1 tools/go.mod tools/go.sum

errdoc: tools
	@echo "generator errors.toml"
	./tools/check-errdoc.sh

failpoint-enable: tools
	tools/bin/failpoint-ctl enable

failpoint-disable: tools
	tools/bin/failpoint-ctl disable

.PHONY: tools web
