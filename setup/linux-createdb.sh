#!/bin/sh
# Run as the local database administrator after installing PostgreSQL.
# The interactive password command does not put a credential in argv or git.
set -eu
createuser --login taxapp
createdb --owner=taxapp taxappdb
psql --dbname=taxappdb --command='\password taxapp'
