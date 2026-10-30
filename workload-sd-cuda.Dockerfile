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

# The architectures to emit machine code for, and the list is load-bearing.
#
# It used to be "86;89" -- 3090/A10/A5000 and 4090/L40S -- chosen because
# naming a few keeps the compile to minutes instead of an hour. The trouble is
# that every card worth renting for a large model was outside it. An A100 is
# sm_80, an H100 sm_90, Blackwell sm_100 or sm_120, and this repository's own
# dev machine is a 2080 Ti at sm_75: the image could not run on the machine
# that built it.
#
# PTX does not save you. CMake emits virtual code for the highest entry only,
# and PTX JITs *forward*, never back -- so compute_89 covers Blackwell and
# cannot touch an A100. A run on an 80 GiB A100 therefore did not fail. It
# fell back to the CPU backend and started a 32-billion-parameter model on 128
# threads, at the GPU's price. See internal/gpucheck.
#
# 80 is here for the A100 and 90 for the H100; 86 and 89 stay for Ampere
# consumer and Ada. sm_75 is deliberately *not* here: a 2080 Ti has 11 GiB,
# which cannot hold the models this image exists to run, and the CPU variant
# covers working locally. So the sentence above stays true -- this image still
# will not run on the machine that builds it -- and that is now a decision
# rather than an accident. CMake also emits virtual code for the highest entry, so
# the binary carries compute_90 PTX -- which is what leaves Blackwell a
# forward-JIT path until the base image is new enough to build sm_120
# natively (that needs CUDA 12.8+; this is 12.6.3). The lab reads the markers
# back out of the binary rather than trusting that sentence.
#
# Adding architectures costs build time, which is why this was narrow for so
# long. The GitHub cache added in .github/workflows/workload-image.yml is what
# makes the list affordable: unchanged, this whole stage comes back from cache.
#
# test/integration/imagelab_test.go asserts that what ends up in the binary
# actually covers the cards ember can select, so this cannot quietly drift
# again.
ARG CUDA_ARCHS="80;86;89;90"
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

# No model. This image is code, and the weights live on a store.
#
# They used to be COPY'd in, and the comment said that baking them avoided
# billing the download at GPU rates. That was true and it was the wrong trade
# at any size worth having. A COPY'd model gets a layer digest that is ours
# alone, so it can never hit a shared cache on any host -- the identical bytes
# sit on the same machines under somebody else's digest, and we re-download
# ours on every pod. The checkpoint was 51% of this image and 92% of what a
# one-shot run cost. Above about 10 GB it is not even possible: the registry
# refuses a layer larger than that, and Wan 2.2's experts are 14 GB each.
#
# `ember store push` puts them on a volume once, for the price of the storage
# and no compute, and `assets:` names them with a digest so the node verifies
# what it mounts before anything expensive runs.
#
# The mount point is not baked in either: the config names it, node.MountAt
# refuses the paths that would break the node, and the workload's own command
# refers to it.
RUN mkdir -p /workspace/out

EXPOSE 22
ENTRYPOINT ["/usr/local/bin/embernode"]
