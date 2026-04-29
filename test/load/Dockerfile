FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends dnsperf \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /work
ENTRYPOINT ["dnsperf"]
