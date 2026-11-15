#!/bin/sh
### must run this script AFTER installing postgres

# switch to postgres user
su - postgres
# this creates a postgres user then database
createuser taxapp
createdb taxappdb
# command line session with postgres tool
psql
alter user taxapp with encrypted password '${PRESERVATION_REDACTED}';
grant all privileges on database taxappdb to taxapp;

#now confirm the DB is there
select datname from pg_database where datistemplate = false;


# While developing you might wanna delete everything...
#delete from tx_addresses;
#delete from addresses;
#delete from txes;
#delete from blocks;
#commit;
