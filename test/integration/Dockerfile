# ember:lab — the node image.
#
# This is what ember starts when the local provider rents a "machine". It runs
# sshd and nothing else that matters, so every layer above the provider — the
# handshake, the host-key pin, the forward, the teardown — exercises its real
# production code path against a real sshd, for no money.
#
# Pinned by digest, not by tag. A tag is mutable upstream, and a lab whose base
# image changed under it reports a failure in ember for a change nobody here
# made.
FROM golang:1.27-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS build
WORKDIR /src
# Modules first, so a source-only change does not re-download the world.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off so the binaries are static and the runtime stage needs no toolchain.
ENV CGO_ENABLED=0
RUN go build -o /out/embernode ./cmd/embernode \
 && go build -o /out/emberwl  ./cmd/emberwl

FROM debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

# openssh-server is the point of the image. ca-certificates and curl are for
# workloads that fetch something; iproute2 and procps are for diagnosing a lab
# that misbehaves, which is otherwise done blind.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      openssh-server ca-certificates curl iproute2 procps \
 && rm -rf /var/lib/apt/lists/*

# sshd refuses to start without this directory, and the package does not always
# create it.
RUN mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh

# Key-only, root login by key, no passwords at all. The node is reachable from
# the internet on a rented machine, so this is the configuration that matters
# most in the whole repository.
RUN printf '%s\n' \
      'PermitRootLogin prohibit-password' \
      'PasswordAuthentication no' \
      'KbdInteractiveAuthentication no' \
      'ChallengeResponseAuthentication no' \
      'PubkeyAuthentication yes' \
      'AuthorizedKeysFile /root/.ssh/authorized_keys' \
      'AllowTcpForwarding yes' \
      'X11Forwarding no' \
      'PrintMotd no' \
      'UseDNS no' \
    > /etc/ssh/sshd_config.d/ember.conf

# Remove any host keys the image shipped with. ember injects the one it minted,
# and a leftover key would be one ember never pinned — the connection would
# fail with a mismatch that looks like an attack.
RUN rm -f /etc/ssh/ssh_host_*

COPY --from=build /out/embernode /usr/local/bin/embernode
COPY --from=build /out/emberwl   /usr/local/bin/emberwl

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/embernode"]
