#!/bin/bash

THIS_SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"

if [[ "$GOPATH" == "" ]]; then
  1>&2 echo "You must define GOPATH to use this script!"
  exit 1
fi


DVID_REPO=${THIS_SCRIPT_DIR}
cd ${DVID_REPO}

DEFAULT_BACKENDS="basholeveldb gbucket"
DVID_BACKENDS=${DVID_BACKENDS-${DEFAULT_BACKENDS}}

if [[ "${DVID_BACKENDS}" == "" ]]; then
    1>&2 echo "Error: No backend selected!"
    1>&2 echo "       Specify DVID_BACKENDS or unset it to use the default (${DEFAULT_BACKENDS})"
    exit 1
fi

export CGO_CFLAGS="-I${PREFIX}/include"
export CGO_LDFLAGS="-L${PREFIX}/lib"

# Build nrsc -- is it unused?
#cd ${GOPATH}/src/github.com/janelia-flyem/go/nrsc/nrsc
#go build -o ${PREFIX}/nrsc
#cd -

set -x

make dvid

## dvid-gen-version
#go build -o ${PREFIX}/bin/dvid-gen-version -v -tags "${DVID_BACKENDS}" cmd/gen-version/main.go 
#
## Build DVID
#${PREFIX}/bin/dvid-gen-version -o ${DVID_REPO}/server/version.go
#go build -o ${PREFIX}/bin/dvid -v -tags "${DVID_BACKENDS}" ${DVID_REPO}/cmd/dvid/main.go
#
## dvid-backup
#go build -o ${PREFIX}/bin/dvid-backup -v -tags "${DVID_BACKENDS}" cmd/backup/main.go 
#
## dvid-transfer
#go build -o ${PREFIX}/bin/dvid-transfer -v -tags "${DVID_BACKENDS}" cmd/transfer/*.go

