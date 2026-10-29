# An ember workload image: a node that can generate an image.
#
# Built on the node image, because ember runs *one* container that is both the
# machine and the work. It therefore needs sshd and embernode alongside
# whatever it was rented to do.
#
# stable-diffusion.cpp rather than ComfyUI or diffusers, deliberately: it is
# C/C++ with no Python and no PyTorch, which keeps this project's rule intact,
# and the whole image is a few gigabytes rather than a dozen. On a rented GPU
# that difference is pull time, and pull time is billed.
#
# This is the CPU variant, for proving the pipeline locally for nothing. The
# CUDA variant is a separate build: upstream publishes no prebuilt Linux CUDA
# binary, so that one compiles against the CUDA toolkit.
ARG NODE_IMAGE=ember:lab
FROM ${NODE_IMAGE}

# Pinned to a release tag and verified by digest. A moving download is a node
# whose behaviour changes without anything in this repository changing, which
# is the same reason the base images are pinned by digest.
ARG SD_RELEASE=master-1024-77b72a2
ARG SD_ASSET=sd-master-77b72a2-bin-Linux-Ubuntu-24.04-x86_64.zip
ARG SD_SHA256=d44a17e278412bd45c1847a91d30a69146b9bb9144c91a61fa0e357d7e2466af

ADD --checksum=sha256:${SD_SHA256} \
    https://github.com/leejet/stable-diffusion.cpp/releases/download/${SD_RELEASE}/${SD_ASSET} \
    /tmp/sd.zip

# libgomp1 is OpenMP, which the ggml CPU backends link against. It is absent
# from debian-slim, and without it sd-cli dies at exec with
# "error while loading shared libraries: libgomp.so.1". Installed rather than
# discovered on a rented GPU, where the same failure would cost a provision.
RUN apt-get update \
 && apt-get install -y --no-install-recommends unzip libgomp1 \
 && mkdir -p /opt/sd \
 && unzip -q /tmp/sd.zip -d /opt/sd \
 && rm -f /tmp/sd.zip \
 && apt-get purge -y unzip \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# The shared objects live beside the binary, so the loader is told where to
# look rather than relying on an LD_LIBRARY_PATH the SSH session would not
# inherit: a non-interactive `ssh host command` gets a very small environment,
# and a model that loads from a terminal and not from ember is a confusing
# afternoon.
RUN printf '/opt/sd\n' > /etc/ld.so.conf.d/ember-sd.conf && ldconfig \
 && ln -s /opt/sd/sd-cli /usr/local/bin/sd-cli

# The model is baked in rather than fetched at run time. Downloading it on a
# rented GPU would bill the download at GPU rates, and a node that reaches out
# to the internet on startup is a node whose run depends on somebody else's
# uptime.
ARG MODEL_PATH=sd15.gguf
COPY ${MODEL_PATH} /opt/models/sd15.gguf

RUN mkdir -p /workspace/out
