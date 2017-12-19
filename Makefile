
#PREFIX = ${PREFIX}

bin/dvid-gen-version: cmd/gen-version/main.go
	go build -o bin/dvid-gen-version -v -tags "${DVID_BACKENDS}" cmd/gen-version/main.go 

.PHONY: compile-version
compile-version: bin/dvid-gen-version
	bin/dvid-gen-version -o ${DVID_REPO}/server/version.go


# FIXME: This finds ALL go source files, not just the selection of sources that are needed for dvid.
DVID_SOURCES = $(shell find . -name "*.go")

bin/dvid: $(DVID_SOURCES)
	go build -o bin/dvid -v -tags "${DVID_BACKENDS}" cmd/dvid/main.go

dvid: bin/dvid

bin/dvid-backup: cmd/backup/main.go
	go build -o bin/dvid-backup -v -tags "${DVID_BACKENDS}" cmd/backup/main.go 

dvid-backup: bin/dvid-backup

bin/dvid-transfer: $(shell find cmd/transfer "*.go")
	go build -o bin/dvid-transfer -v -tags "${DVID_BACKENDS}" cmd/transfer/*.go

dvid-transfer: bin/dvid-transfer
