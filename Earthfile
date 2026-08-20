# See https://docs.earthbuild.dev/docs/earthfile/features
VERSION --try --raw-output 0.8

ARG --global GO_VERSION=1.26.0
# Pinned, not @latest. golangci-lint hard-errors on an unknown linter name, and
# releases rename linters (2.13.0: exhaustruct -> exhaustruct_v5), so a floating
# version means an upstream release can fail CI with no change on our side —
# and makes local lint disagree with CI. Bump this deliberately, together with
# any renamed names in .golangci.yml.
ARG --global GOLANGCI_LINT_VERSION=v2.12.2

# reviewable checks that a branch is ready for review. Run it before opening
# a pull request.
reviewable:
  BUILD +lint
  BUILD +test

# build builds the unjira binary for your native OS and architecture.
build:
  ARG USERPLATFORM
  BUILD --platform=$USERPLATFORM +go-build

# multiplatform-build builds unjira for every supported OS and architecture.
# Set RELEASE_ARTIFACTS=true for the flat release-ready layout the release
# workflow expects (.tar.gz + .sha256 per platform under _output/release/).
multiplatform-build:
  ARG RELEASE_ARTIFACTS=false
  BUILD +go-multiplatform-build --RELEASE_ARTIFACTS=${RELEASE_ARTIFACTS}

# test runs unit tests (the "live" build-tagged tier is excluded by default;
# run it separately with UNJIRA_LIVE=1 and go test -tags=live).
test:
  BUILD +go-test

# lint runs golangci-lint.
lint:
  BUILD +go-lint

# generate tidies go.mod/go.sum. Run explicitly when dependencies change.
generate:
  BUILD +go-modules-tidy

go-deps:
  FROM golang:${GO_VERSION}-bookworm
  WORKDIR /unjira
  COPY go.mod go.sum ./
  RUN go mod download
  COPY . .

go-build:
  FROM +go-deps
  ARG GOOS=linux
  ARG GOARCH=amd64
  ARG BIN_NAME=unjira
  ENV CGO_ENABLED=0
  RUN GOOS=${GOOS} GOARCH=${GOARCH} go build -o ${BIN_NAME} ./cmd/unjira
  SAVE ARTIFACT ${BIN_NAME} AS LOCAL _output/bin/${GOOS}_${GOARCH}/${BIN_NAME}

# Set RELEASE_ARTIFACTS=true to output flat release-ready artifacts to
# _output/release/ instead of the per-platform _output/bin/ layout.
go-multiplatform-build:
  FROM +go-deps
  ARG RELEASE_ARTIFACTS=false
  IF [ "${RELEASE_ARTIFACTS}" = "true" ]
    BUILD +go-release-artifact --GOOS=darwin --GOARCH=amd64
    BUILD +go-release-artifact --GOOS=darwin --GOARCH=arm64
    BUILD +go-release-artifact --GOOS=linux --GOARCH=amd64
    BUILD +go-release-artifact --GOOS=linux --GOARCH=arm64
  ELSE
    BUILD +go-build --GOOS=darwin --GOARCH=amd64
    BUILD +go-build --GOOS=darwin --GOARCH=arm64
    BUILD +go-build --GOOS=linux --GOARCH=amd64
    BUILD +go-build --GOOS=linux --GOARCH=arm64
  END

go-release-artifact:
  FROM +go-deps
  ARG GOOS
  ARG GOARCH
  ARG BIN_NAME=unjira
  ENV CGO_ENABLED=0
  RUN GOOS=${GOOS} GOARCH=${GOARCH} go build -o ${BIN_NAME} ./cmd/unjira
  RUN sha256sum ${BIN_NAME} > ${BIN_NAME}.sha256
  RUN tar czf ${BIN_NAME}.tar.gz ${BIN_NAME}
  RUN sha256sum ${BIN_NAME}.tar.gz > ${BIN_NAME}.tar.gz.sha256
  SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}.tar.gz
  SAVE ARTIFACT --keep-ts ${BIN_NAME}.tar.gz.sha256 AS LOCAL _output/release/${BIN_NAME}_${GOOS}_${GOARCH}.tar.gz.sha256

go-test:
  FROM +go-deps
  RUN go test ./...

go-lint:
  FROM +go-deps
  RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}
  RUN golangci-lint run ./...

go-modules-tidy:
  FROM +go-deps
  RUN go mod tidy
  SAVE ARTIFACT go.mod AS LOCAL go.mod
  SAVE ARTIFACT go.sum AS LOCAL go.sum
