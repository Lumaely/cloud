#!/usr/bin/env bash

docker build -t lumaely-cloud:latest .

docker run -d --name lumaely --env-file .env -p 8080:8080 lumaely-cloud:latest

echo -ne "To look at the logs, run:\n"
echo -ne "\tdocker logs -f lumaely\n"
