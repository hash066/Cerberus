FROM mcr.microsoft.com/devcontainers/go:1.25-bookworm

ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:/go/bin:${PATH}

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        curl \
        pkg-config \
    && rm -rf /var/lib/apt/lists/*

RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --no-modify-path --profile default --default-toolchain stable --component clippy --component rustfmt \
    && chmod -R a+w ${RUSTUP_HOME} ${CARGO_HOME}

RUN go install github.com/go-task/task/v3/cmd/task@latest \
    && go install github.com/goreleaser/goreleaser/v2@latest
