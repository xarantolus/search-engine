FROM ubuntu:24.04

RUN --mount=type=cache,target=/var/cache/apt/archives \
	apt-get update && \
	DEBIAN_FRONTEND=noninteractive \
	apt-get remove -y golang-go && \
	apt-get install -y \
	pkg-config libssl-dev \
	davfs2 cifs-utils samba git git-lfs \
	nodejs npm \
	curl podman podman-compose \
	python3 python3-pip \
	&& \
	apt-get clean && \
	rm -rf /var/lib/apt/lists/*

RUN set -e; \
	arch="$(dpkg --print-architecture)"; \
	case "$arch" in \
	amd64) go_arch="amd64";; \
	arm64) go_arch="arm64";; \
	*) echo "Unsupported architecture: $arch" && exit 1;; \
	esac; \
	version="$(curl -s 'https://go.dev/VERSION?m=text' | head -n1)"; \
	curl -fsSL "https://go.dev/dl/${version}.linux-${go_arch}.tar.gz" -o "/tmp/go.tar.gz"; \
	tar -C /usr/local -xzf "/tmp/go.tar.gz"; \
	rm "/tmp/go.tar.gz"

ENV PATH="/usr/local/go/bin:${PATH}"

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y && \
	echo 'source $HOME/.cargo/env' >> $HOME/.bashrc && \
	. $HOME/.cargo/env

RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin latest

ENV PATH="/root/go/bin:${PATH}"
