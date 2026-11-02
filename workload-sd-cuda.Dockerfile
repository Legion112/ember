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
FROM nvidia/cuda:13.0.1-devel-ubuntu24.04@sha256:7d2f6a8c2071d911524f95061a0db363e24d27aa51ec831fcccf9e76eb72bc92 AS sdbuild

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

# The architecture to emit machine code for: exactly one, named by the caller.
#
# One image per architecture, and no default here. Both halves are deliberate.
#
# This used to be a list, `CUDA_ARCHS="80;86;89;90"`, producing one fat image.
# Two things were wrong with that. Every card outside the list ran on the CPU
# backend at the GPU's price rather than failing -- an 80 GiB A100 did exactly
# that, starting a 32-billion-parameter model on 128 threads (see
# internal/gpucheck) -- and the list was kept narrow because every entry costs
# build time: measured on a four-vCPU runner, two architectures took 1846s and
# four took 4264s. So the narrow list and the slow build held each other in
# place, and the cards worth renting were the ones left out.
#
# A build matrix breaks that: one architecture per job, so the wall clock is
# one compile rather than seven, and the list stops being bounded by a
# timeout. What arrives on a node is then chosen by ember from the `images:`
# map, for the architecture of the card it actually rented, and a card with no
# entry is refused before anything is billed.
#
# No default, because a default is how a path that forgot the build arg
# produces a wrong-architecture image that looks correct. Unset fails in the
# RUN below, naming the make target that sets it. `CUDA_ARCHS` plural is gone
# rather than kept for compatibility: a stale caller passing it gets docker's
# unconsumed-build-arg warning and then this guard, which is the behaviour you
# want from a renamed knob.
ARG CUDA_ARCH

# "-real", which is what emits no PTX at all.
#
# A bare "89" tells CMake to emit virtual code for the entry as well, so the
# binary carried compute_NN PTX. That PTX is not a safety net worth having: it
# JITs forward only, so it could never help an older card, and on a newer one
# it means the driver compiles 150 kernel modules at first launch, on a cold
# cache, on the most expensive hardware in the catalogue, every single pod --
# unmeasured, and reported by nothing.
#
# Nothing is compiled on a rented machine. A card this image cannot run is
# refused by ember before the rental rather than JITed through, and dropping
# the PTX is what makes that the only outcome. It also lets the image lab
# assert a sharp property -- exactly one sm_NN and no compute_* -- instead of
# the t.Logf it used to forgive the JIT case with.
#
# test/integration/imagelab_test.go reads both back out of the binary rather
# than trusting this comment.
RUN test -n "${CUDA_ARCH}" || { \
      echo "CUDA_ARCH is not set: this image is built for exactly one architecture."; \
      echo "  make workload-sd-cuda CUDA_ARCH=89      # 4090 / L40S"; \
      echo "  make workload-sd-cuda-local             # whatever is in this machine"; \
      echo "ember archs --cards lists every one ember publishes."; \
      exit 1; }; \
    cd /src \
 && cmake -B build -DSD_CUDA=ON -DCMAKE_BUILD_TYPE=Release \
      -DCMAKE_CUDA_ARCHITECTURES="${CUDA_ARCH}-real" \
      -DGGML_NATIVE=OFF -DGGML_AVX2=ON -DGGML_FMA=ON -DGGML_F16C=ON \
 && cmake --build build --config Release -j"$(nproc)" \
 && mkdir -p /out \
 && find build -name 'sd-cli' -type f -exec cp {} /out/ \; \
 && find build -name 'sd-server' -type f -exec cp {} /out/ \; \
 && find build -name '*.so*' -type f -exec cp -P {} /out/ \; \
 && { test -x /out/sd-cli || { echo "the build produced no sd-cli"; exit 1; }; } \
 && { test -x /out/sd-server || { echo "the build produced no sd-server; examples/CMakeLists.txt is supposed to add_subdirectory(server) unconditionally at this revision"; exit 1; }; }

# sd-server was already being compiled and thrown away.
#
# examples/CMakeLists.txt at this revision is two unconditional lines --
# add_subdirectory(cli) and add_subdirectory(server) -- so `cmake --build`
# has always produced both and this image only ever copied one out. Taking
# the second costs no build time and no revision bump.
#
# It is what makes a resident model possible: every entry in `cmds` is a
# fresh sd-cli that reads the weights into VRAM again, and on a rented A100
# that is about twelve seconds per image during which nothing is generating.
# A server loads them once. See internal/sdserver.
#
# Both binaries are checked rather than assumed, because `find -exec cp`
# succeeds when it matches nothing: without the test, an upstream revision
# that moved or renamed the target would publish an image whose only symptom
# is "sd-server: not found" over SSH, after a node was rented.
#
# Each test is braced. Unbraced, `A && test -x x || { echo; exit 1; }` is
# parsed left to right, so a cmake failure skips the test, falls into the
# block and reports "the build produced no sd-cli" -- blaming the copy for a
# compile error, at the end of a forty-minute build.

# GGML_NATIVE=OFF, because ON compiles -march=native -- for the machine doing
# the build.
#
# ggml defaults it ON, so the published image was compiled for whatever CPU the
# GitHub runner happened to have. Measured from the same Dockerfile and the
# same source:
#
#   built on a Zen 3 5950X (no AVX-512)   0 zmm references
#   built on a GitHub runner              23,458 zmm references
#
# A binary full of AVX-512 runs on the hosts that have it and dies on the ones
# that do not. On a rented RTX A4000 this was:
#
#   ggml_cuda_init: found 1 CUDA devices ... compute capability 8.6
#   Illegal instruction (core dumped)
#
# which is a SIGILL on the host CPU and has nothing to do with CUDA -- an
# earlier run of the same tag on a different host had worked. So the same
# digest behaved differently on two machines, which is worse than the :latest
# problem pinning exists to prevent: there, two runs ran different software;
# here, the identical bytes do different things and nothing in the reference
# can warn you.
#
# AVX2, FMA and F16C are Haswell, 2013, and universal on anything rentable.
# The CPU path barely matters here anyway -- the work is on the GPU -- so a
# baseline that runs everywhere is the right trade.

# What nvcc actually emitted, checked here rather than asserted later.
#
# The architecture markers used to be readable as ASCII in the binary, and the
# image lab grepped for them. They were there because embedded PTX is *text*:
# a PTX module carries ".target sm_75" in plain characters. With -real there is
# no PTX, so there are no strings -- the absence is the evidence that the PTX
# is gone, and it is also why grep can no longer say which architecture the
# SASS is for.
#
# cuobjdump can, and it is in this stage because this stage has the toolkit.
# So the check happens where the answer is available and the failure is free:
# a wrong-architecture image cannot be built rather than being built and then
# caught. The image lab still compares the label against what the caller asked
# for, because a label can drift from the binary it describes.
RUN set -eu; \
    elf=$(cuobjdump --list-elf /out/sd-cli | grep -oE 'sm_[0-9]+' | sort -u | paste -sd' ' -); \
    ptx=$(cuobjdump --list-ptx /out/sd-cli | grep -oE 'sm_[0-9]+' | sort -u | paste -sd' ' - || true); \
    echo "SASS: ${elf:-none}"; \
    echo "PTX:  ${ptx:-none}"; \
    if [ "$elf" != "sm_${CUDA_ARCH}" ]; then \
      echo "this image was asked for sm_${CUDA_ARCH} and the binary carries [${elf:-none}]."; \
      echo "A card with no matching kernel aborts on the first launch -- \"no kernel"; \
      echo "image is available for execution on the device\" -- but only after a node has"; \
      echo "been rented, the image pulled and the weights fetched. So it is not published."; \
      exit 1; \
    fi; \
    command -v objdump >/dev/null 2>&1 || { \
      echo "objdump is missing, so the AVX-512 check cannot run."; \
      echo "It is not optional: a check that skips itself reports success for something"; \
      echo "that never ran, and what it guards is a binary that dies with SIGILL on"; \
      echo "hosts the build machine does not resemble."; \
      exit 1; }; \
    zmm=$(objdump -d --no-show-raw-insn /out/sd-cli | grep -coE '%zmm[0-9]+' || true); \
    echo "AVX-512: ${zmm} zmm references"; \
    if [ "${zmm}" != "0" ]; then \
      echo "the binary uses AVX-512, which not every host that rents a GPU has."; \
      echo "A host without it does not run slowly, it dies: SIGILL, 'Illegal instruction',"; \
      echo "after CUDA has already initialised and with nothing naming the CPU as the cause."; \
      echo "GGML_NATIVE must be OFF: ON means -march=native, for the build machine."; \
      exit 1; \
    fi; \
    if [ -n "$ptx" ]; then \
      echo "the binary carries PTX for [${ptx}] and this image ships none."; \
      echo "PTX JITs forward only: it cannot help an older card, and on a newer one it"; \
      echo "means the driver compiles every kernel at first launch, on a cold cache,"; \
      echo "on every pod. Nothing is compiled on a rented machine."; \
      echo "CMAKE_CUDA_ARCHITECTURES must be \"NN-real\"; a bare NN emits PTX too."; \
      exit 1; \
    fi

# ---- the node itself -------------------------------------------------------
# CUDA 13.0.1, and the version is a measurement rather than a preference.
#
# 12.6.3 could not emit code for Blackwell at all: CUDA_ARCH=100 and =120 both
# fail in two seconds with "nvcc fatal: Unsupported gpu architecture", so the
# B200 and the RTX PRO 6000 had no image that could run on them. sm_100 and
# sm_120 need 12.8 at the earliest.
#
# 13.x rather than 12.8 because the host driver allows it, which is the only
# thing that decides it. A CUDA 13 binary needs roughly a 580 driver, and a
# probe of a rented RTX 4090 reported:
#
#   NVIDIA-SMI 595.91.07   Driver Version: 595.91.07   CUDA Version: 13.2
#
# so the fleet is ahead of the toolkit, with headroom. That probe cost
# $0.004866. CUDA 13.0 also makes Turing the *minimum* supported architecture
# -- it dropped Maxwell, Pascal and Volta -- so sm_75 is still buildable and
# nothing ember publishes an image for was lost.
#
# The driver is not in this image and cannot be: the container runtime injects
# libcuda.so.1 from the host, which is why that probe was the question. What
# the image must carry is the CUDA *runtime*, and its sonames move with the
# major version -- libcudart.so.13 now, not .so.12. Anything reasoning about
# which libraries this image actually links has to be re-measured.
#
# -base, not -runtime, because measured on the image this file produced:
# 87% of what a node downloads was CUDA libraries nothing in it links.
#
# `nvidia/cuda:*-runtime` is `*-base` plus one apt layer, `cuda-libraries-13-0`
# and `libnccl2`. On sm_75 that layer is 2117.7 MB uncompressed and **1496.7 MB
# of a 1724.1 MB pull**. What sd-cli and sd-server link out of it is cublas
# (51.9 MiB) and cublasLt (514.5 MiB) from the toolkit and nccl (180.9 MiB)
# beside it. The other ~1264 MiB is shipped and never opened:
#
#   libcufft 273.3   libcusparse 155.0   libcusolver 134.6   libcurand 126.6
#   libnvrtc 104.3   libnvrtc.alt 104.7  libcusolverMg 99.1  libnvJitLink 94.2
#   the NPP family ~151   libnvjpeg 5.6   libcufile 3.0   libnvfatbin 2.4
#
# ldd and the string table agree about that, which is what made it actionable:
# the complete set of `.so` names appearing as literals anywhere in either
# binary is exactly its DT_NEEDED set, so no code path can name cufft or nvrtc
# at runtime either. test/integration/imagelab_test.go asserts the ldd half on
# every build, because that is the half this swap can break.
#
# libcudart is *not* copied, and that is not an omission: -base already ships
# libcudart.so.13 (0.7 MiB) -- it is the one CUDA library it exists to carry.
# A COPY of it would be a second copy of a file already there.
#
# COPY from sdbuild rather than `apt install libcublas-13-0 libnccl2`, because
# sdbuild is the stage that compiled the binary and therefore holds the exact
# build the binary was linked against -- 13.0.2.14 and 2.28.3 here, verified
# equal to what -runtime shipped. An apt index moves; a copy out of the stage
# above cannot. Same reason every base here is pinned by digest.
#
# A `RUN rm` of the unused libraries would save nothing. Layers are additive:
# the bytes stay in the layer below and the pull still carries them. The base
# has to change, which is why this is a FROM line and not a cleanup step.
#
# Nothing about PTX JIT changes in either direction. /usr/local/cuda-13.0/compat
# -- the forward-compatibility package, 307 MiB of it -- is in -base as well,
# and the JIT compiler itself comes from the host driver. Nothing is compiled
# on a rented machine regardless: this image carries SASS for one architecture
# and no PTX, and a card it has no kernel for is refused before the rental.
#
# What is lost is /opt/nvidia/nvidia_entrypoint.sh, which -runtime sets as its
# ENTRYPOINT and -base does not have. It never ran: this image's ENTRYPOINT is
# embernode, set at the bottom of this file, which replaced it.
#
# -runtime is the vendor-supported shape and a hand-picked list is not, so a
# CUDA bump has to re-read `ldd` rather than assume this list still holds. That
# is a lab assertion rather than a promise: see
# TestWorkloadImage_LinksNothingItDoesNotShip.
FROM nvidia/cuda:13.0.1-base-ubuntu24.04@sha256:f8ef28f579ea42a44b415d2c5d46f788e6a9b395c6c83f2929416e1fc192c143

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

# The three CUDA libraries the binaries above actually link, and no others.
#
# They go where the vendor packages put them, because both directories are
# already on the loader's search path in -base: /etc/ld.so.conf.d lists
# /usr/local/cuda/targets/x86_64-linux/lib and /usr/lib/x86_64-linux-gnu. The
# alternative -- dropping them in /opt/sd beside the binaries -- would work and
# would also mean the one directory this image adds to the search path holds
# both ember's build and NVIDIA's.
#
# The glob takes the *versioned* file and deliberately not the soname symlink
# beside it. `COPY --from` dereferences a symlink rather than copying the link,
# so `libcublas.so.13*`, which matches both, copied the 51.9 MiB file twice and
# the 514.5 MiB one twice -- 747 MiB of duplicates, an image 2.20 GB instead of
# 1.41 GB, and `ldconfig` complaining that "libcublas.so.13 is not a symbolic
# link" at the end of the build. Measured, not reasoned about: the first version
# of this block did exactly that. The `.` after the major version is the whole
# fix, and it is why the pattern looks oddly specific.
#
# The soname links are made by the ldconfig below, which is what ldconfig is
# for: it reads SONAME out of each file and creates the name DT_NEEDED asks
# for. That step already existed for /opt/sd.
#
# The list is three entries long because that is what `ldd` reports, not
# because three felt like enough. A fourth would be a library the loader
# cannot resolve, and TestWorkloadImage_LinksNothingItDoesNotShip fails naming
# it on a laptop with no GPU, before anything is rented.
COPY --from=sdbuild /usr/local/cuda/targets/x86_64-linux/lib/libcublas.so.13.* \
                    /usr/local/cuda/targets/x86_64-linux/lib/
COPY --from=sdbuild /usr/local/cuda/targets/x86_64-linux/lib/libcublasLt.so.13.* \
                    /usr/local/cuda/targets/x86_64-linux/lib/
COPY --from=sdbuild /usr/lib/x86_64-linux-gnu/libnccl.so.2.* \
                    /usr/lib/x86_64-linux-gnu/

# What architecture this image is for, said by the image itself.
#
# Repeated in the runtime stage because an ARG does not cross a FROM. Both
# spellings, for two different readers:
#
# The LABEL travels with the image into the registry, so a published artifact
# can be asked what it is without being run. The image lab compares it against
# the architecture the build was *asked* for, which arrives from outside the
# build as an environment variable -- so a build arg that silently stopped
# taking effect is caught by a disagreement rather than by nobody noticing.
#
# The ENV is readable from inside a running container, which makes it
# something gpu_probe can compare against the card it finds. That closes the
# one failure an `images:` map cannot prevent: an operator pasting the right
# digest under the wrong key. ember would then run a wrong-architecture image
# believing it correct, and the probe is the only thing left that can say so
# -- for the price of the probe and no GPU time.
ARG CUDA_ARCH
LABEL ember.cuda-arch="${CUDA_ARCH}"
ENV EMBER_CUDA_ARCH="${CUDA_ARCH}"
RUN printf '%s\n' "${CUDA_ARCH}" > /etc/ember-cuda-arch

# Three places, and the third is the only one a workload can read.
#
# The LABEL travels with the image into a registry, so a published artifact can
# be asked what it is without being run; the image lab compares it against what
# the build was asked for.
#
# The ENV is visible to this image's entrypoint and to `docker run`.
#
# The FILE is what a gpu_probe or a workload can actually see, and it exists
# because the ENV is not enough -- which this Dockerfile already knew three
# lines further down, about LD_LIBRARY_PATH. ember runs the probe and the
# workload as `ssh host command`, and sshd builds a fresh environment for each
# session: the image's ENV reaches PID 1 and nothing sshd forks. Measured on a
# rented RTX 2000 Ada, where the image carries EMBER_CUDA_ARCH=89 and the
# workload printed "image says EMBER_CUDA_ARCH=".
#
# That mattered more than a blank line of output. A probe comparing the card
# to an empty string refuses *every* run, including correct ones -- so the
# mechanism meant to catch a wrong-architecture image would instead have
# rejected the right one.

# The loader is told where the shared objects are rather than relying on
# LD_LIBRARY_PATH: a non-interactive `ssh host command` gets a very small
# environment, and a model that loads from a terminal but not from ember is a
# confusing afternoon.
RUN printf '/opt/sd\n' > /etc/ld.so.conf.d/ember-sd.conf && ldconfig \
 && ln -sf /opt/sd/sd-cli /usr/local/bin/sd-cli \
 && ln -sf /opt/sd/sd-server /usr/local/bin/sd-server

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
