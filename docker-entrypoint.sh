#!/bin/bash
set -e

# Generate /etc/bucardorc dynamically from environment variables
echo "Generating /etc/bucardorc..."
cat << EOF > /etc/bucardorc
dbhost = ${BUCARDO_DB_HOST:-postgres}
dbport = ${BUCARDO_DB_PORT:-5432}
dbuser = ${BUCARDO_DB_USER:-postgres}
dbname = ${BUCARDO_DB_NAME:-bucardo}
dbpass = ${BUCARDO_DB_PASS:-changeme}
EOF

# Change ownership to the user that will run the bucardo command
chown postgres:postgres /etc/bucardorc
echo "Set ownership of /etc/bucardorc to postgres"

# Ensure the run directory exists for Bucardo PID files
mkdir -p /var/run/bucardo
chown postgres:postgres /var/run/bucardo
echo "Created /var/run/bucardo and set ownership"

# Ensure the log directory exists
mkdir -p /var/log/bucardo
touch /var/log/bucardo/log.bucardo
chown -R postgres:postgres /var/log/bucardo
echo "Created /var/log/bucardo and set ownership"

# Execute the main Go application
exec /entrypoint
