#!/bin/sh
set -eu

data_dir="${THOUGHTGLEAN_DATA_DIR:-/data}"
backup_dir="${THOUGHTGLEAN_BACKUP_DIR:-$data_dir/backups}"
mkdir -p "$data_dir"

data_uid="$(stat -c '%u' "$data_dir")"
data_gid="$(stat -c '%g' "$data_dir")"

# A fresh named volume inherits /data's image ownership. A bind mount keeps
# the host directory owner; running as that numeric identity lets SQLite
# create its database, WAL and lock files without making the directory 0777.
if [ "$data_uid" = "0" ]; then
  data_uid=10001
  data_gid=10001
  chown -R "$data_uid:$data_gid" "$data_dir"
fi

mkdir -p "$backup_dir"
backup_uid="$(stat -c '%u' "$backup_dir")"
if [ "$backup_uid" = "0" ]; then
  chown -R "$data_uid:$data_gid" "$backup_dir"
elif [ "$backup_uid" != "$data_uid" ]; then
  echo "backup directory owner does not match data directory owner" >&2
  echo "expected uid $data_uid, found uid $backup_uid: $backup_dir" >&2
  exit 1
fi

if [ -e "$data_dir/thoughtglean.db" ]; then
  db_uid="$(stat -c '%u' "$data_dir/thoughtglean.db")"
  if [ "$db_uid" != "$data_uid" ]; then
    echo "database owner does not match data directory owner" >&2
    echo "expected uid $data_uid, found uid $db_uid: $data_dir/thoughtglean.db" >&2
    exit 1
  fi
fi

exec gosu "$data_uid:$data_gid" /app/thoughtglean "$@"
