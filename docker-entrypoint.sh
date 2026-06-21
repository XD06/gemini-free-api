#!/bin/sh
set -eu

mkdir -p /home/appuser/data /home/appuser/.cookies
chown -R appuser:appgroup /home/appuser/data /home/appuser/.cookies

exec su-exec appuser:appgroup /home/appuser/main
