#
# Copyright (C) 2017 Canonical Ltd
#
# This program is free software: you can redistribute it and/or modify
# it under the terms of the GNU General Public License version 3 as
# published by the Free Software Foundation.
#
# This program is distributed in the hope that it will be useful,
# but WITHOUT ANY WARRANTY; without even the implied warranty of
# MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
# GNU General Public License for more details.
#
# You should have received a copy of the GNU General Public License
# along with this program.  If not, see <http://www.gnu.org/licenses/>.

SNAPD_ENVIRONMENT_FILE := /etc/environment
SNAP_MOUNT_DIR ?= /snap
BINDIR := /usr/bin
LIBEXECDIR := /usr/lib
SYSTEMDSYSTEMUNITDIR := /lib/systemd/system

SYSTEMD_UNITS_GENERATED := $(wildcard *.in)
SYSTEMD_UNITS = $(SYSTEMD_UNITS_GENERATED:.in=) $(wildcard *.timer) $(wildcard *.socket)

# The special mount unit for "/snap" must be named after the path and the path
# is a variable.  Compute it in make for simplicity. This is equivalent to
# calling systemd-escape --path $(SNAP_MOUNT_DIR)
snap_mount_unit=$(subst /,-,$(patsubst %/,%,$(patsubst /%,%,$(SNAP_MOUNT_DIR)))).mount

# Yes, we want the extra unit too, thank you
SYSTEMD_UNITS += $(snap_mount_unit)

.PHONY: all
all: $(SYSTEMD_UNITS)

.PHONY: install
install: $(filter-out snap.mount,$(SYSTEMD_UNITS)) $(snap_mount_unit)
	# NOTE: old (e.g. 14.04) GNU coreutils doesn't -D with -t
	install -d -m 0755 $(DESTDIR)/$(SYSTEMDSYSTEMUNITDIR)
	install -m 0644 -t $(DESTDIR)/$(SYSTEMDSYSTEMUNITDIR) $^
	install -d -m 0755 $(DESTDIR)/$(LIBEXECDIR)/snapd
	install -m 0755 -t $(DESTDIR)/$(LIBEXECDIR)/snapd snapd.core-fixup.sh

.PHONY: clean
clean:
	rm -f $(SYSTEMD_UNITS_GENERATED:.in=) $(snap_mount_unit)

%: %.in
	cat $< | \
		sed s:@libexecdir@:$(LIBEXECDIR):g | \
		sed s:@SNAPD_ENVIRONMENT_FILE@:$(SNAPD_ENVIRONMENT_FILE):g | \
		sed s:@bindir@:$(BINDIR):g | \
		sed s:@SNAP_MOUNT_DIR@:$(SNAP_MOUNT_DIR):g  | \
		cat > $@

# If SNAP_MOUNT_DIR uses non-default location then rename snap.mount
# so that the directory name is encoded in the file name.
ifneq ($(snap_mount_unit),snap.mount)
$(snap_mount_unit): snap.mount
	cp $< $@
endif
