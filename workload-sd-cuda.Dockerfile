# An ember workload image that generates on a GPU.
#
# The CPU variant (workload-sd.Dockerfile) uses an upstream prebuilt binary.
# There is no prebuilt Linux CUDA build, so this one compiles
# stable-diffusion.cpp against the CUDA toolkit, which is why it is a separate
# file rather than an argument to that one.
#
# It does not build on the node image. That one is debian-slim and this needs
# the CUDA runtime, so the sshd setup is repeated here against an NVIDIA base
# and embernode is compiled from this repository in a stage below. The
# duplication is deliberate and small; the alternative is a node image
# carrying two gigabytes of CUDA for every provider that does not need it.
#
# Every base is pinned by digest: a node whose behaviour changes because a tag
# moved upstream is a run nobody can reproduce.

# ---- embernode, the thing that makes a container an ember node -------------
FROM golang:1.27-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0
RUN go build -o /out/embernode ./cmd/embernode

# ---- sd-cli, compiled with CUDA -------------------------------------------
FROM nvidia/cuda:12.6.3-devel-ubuntu24.04@sha256:392c0df7b577ecae17a17f6ba7f2009c217bb4422f8431c053ae9af61a8c148a AS sdbuild

ARG SD_COMMIT=77b72a2

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git cmake build-essential ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Pinned to the same commit as the CPU image's release asset, so the two
# variants are the same program and a result from one is comparable with the
# other.
RUN git clone --recursive https://github.com/leejet/stable-diffusion.cpp /src \
 && cd /src \
 && git checkout ${SD_COMMIT} \
 && git submodule update --init --recursive

# 86 and 89 cover the cards ember is likely to rent: 3090/A10/A5000 at 86 and
# 4090/L40S at 89. Naming them rather than building every architecture keeps
# the compile to minutes instead of an hour.
ARG CUDA_ARCHS="86;89"
RUN cd /src \
 && cmake -B build -DSD_CUDA=ON -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCHS}" \
 && cmake --build build --config Release -j"$(nproc)" \
 && mkdir -p /out \
 && find build -name 'sd-cli' -type f -exec cp {} /out/ \; \
 && find build -name '*.so*' -type f -exec cp -P {} /out/ \;

# ---- the node itself -------------------------------------------------------
FROM nvidia/cuda:12.6.3-runtime-ubuntu24.04@sha256:92906d87596d638d35015c6353053121bd299d25943b875763321653884ba924

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      openssh-server ca-certificates libgomp1 procps iproute2 \
 && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh

# Key-only, root by key, no passwords. This is the configuration that matters
# most on a machine reachable from the internet, so it is spelled out here
# rather than inherited from a base that might change.
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

# Any host key the image shipped with is removed: ember injects the one it
# minted, and a leftover would be a key ember never pinned, failing with a
# mismatch that looks exactly like an attack.
RUN rm -f /etc/ssh/ssh_host_*

COPY --from=gobuild /out/embernode /usr/local/bin/embernode
COPY --from=sdbuild /out/ /opt/sd/

# The loader is told where the shared objects are rather than relying on
# LD_LIBRARY_PATH: a non-interactive `ssh host command` gets a very small
# environment, and a model that loads from a terminal but not from ember is a
# confusing afternoon.
RUN printf '/opt/sd\n' > /etc/ld.so.conf.d/ember-sd.conf && ldconfig \
 && ln -sf /opt/sd/sd-cli /usr/local/bin/sd-cli

# Baked in rather than fetched at run time: downloading weights on a rented
# GPU bills the download at GPU rates, and a node that reaches out on startup
# depends on somebody else's uptime for a run you are paying for.
ARG MODEL_PATH=.model.gguf
COPY ${MODEL_PATH} /opt/models/sd15.gguf

RUN mkdir -p /workspace/out

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/embernode"]
