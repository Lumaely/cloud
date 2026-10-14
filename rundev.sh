#!/usr/bin/env bash

docker build -t lumaely-dev -f Dockerfile.dev .
docker run -it --rm --name lumaely-dev-run -v $(pwd):/app -p 8080:8080 --env-file .env lumaely-dev