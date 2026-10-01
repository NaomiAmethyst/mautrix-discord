#!/bin/sh
set -eu
bridge_uid="${UID:-1337}"
bridge_gid="${GID:-$bridge_uid}"

if [ ! -f /data/config.yaml ]; then
	cp /opt/mautrix-discord/example-config.yaml /data/config.yaml
	chown "$bridge_uid:$bridge_gid" /data/config.yaml
	echo "Created /data/config.yaml. Configure the homeserver, permissions, database, and network.channels, then restart."
	exit 0
fi

if [ ! -f /data/registration.yaml ]; then
	/usr/bin/mautrix-discord -g -c /data/config.yaml -r /data/registration.yaml
	chown "$bridge_uid:$bridge_gid" /data/config.yaml /data/registration.yaml
	echo "Created /data/registration.yaml. Register it with the homeserver, then restart."
	exit 0
fi

cd /data
chown -R "$bridge_uid:$bridge_gid" /data
exec su-exec "$bridge_uid:$bridge_gid" /usr/bin/mautrix-discord "$@"
