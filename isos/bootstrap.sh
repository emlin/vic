#!/bin/bash
# Copyright 2016 VMware, Inc. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Build the bootstrap filesystem ontop of the base

# exit on failure
set -e

if [ -n "$DEBUG" ]; then
    set -x
fi

DIR=$(dirname $(readlink -f "$0"))
. $DIR/base/utils.sh

function usage() {
echo "Usage: $0 -p staged-package(tgz) -b binary-dir -d <activates debug when set>" 1>&2
exit 1
}

while getopts "p:b:d:" flag
do
    case $flag in

        p)
            # Required. Package name
            package="$OPTARG"
            ;;

        b)
            # Required. Target for iso and source for components
            BIN="$OPTARG"
            ;;
        d)
            # Optional. directs script to make a debug iso instead of a production iso.
            debug="$OPTARG"
            ;;
        *)

            usage
            ;;
    esac
done

shift $((OPTIND-1))

# check there were no extra args and the required ones are set
if [ ! -z "$*" -o -z "$package" -o -z "${BIN}" ]; then
    usage
fi

#################################################################
# Above: arg parsing and setup
# Below: the image authoring
#################################################################

PKGDIR=$(mktemp -d)

unpack $package $PKGDIR

#selecting the init script as our entry point.
if [ -v debug ]; then
    export ISONAME="bootstrap-debug.iso"
    cp ${DIR}/bootstrap/bootstrap.debug $(rootfs_dir $PKGDIR)/bin/bootstrap
else
    export ISONAME="bootstrap.iso"
    cp ${DIR}/bootstrap/bootstrap $(rootfs_dir $PKGDIR)/bin/bootstrap
fi

# copy in our components
cp ${BIN}/tether-linux $(rootfs_dir $PKGDIR)/bin/tether
cp ${BIN}/rpctool $(rootfs_dir $PKGDIR)/sbin/

# kick off our components at boot time
cp ${DIR}/bootstrap/tether.service $(rootfs_dir $PKGDIR)/etc/systemd/system/
ln -s /etc/systemd/system/tether.service $(rootfs_dir $PKGDIR)/etc/systemd/system/multi-user.target.wants/tether.service


generate_iso $PKGDIR $BIN/$ISONAME /lib/systemd/systemd