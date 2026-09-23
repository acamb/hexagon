# Hexagon base image.
#
# A session image must provide git, tmux and claude on the PATH, and SHELL
# pointing at an interactive shell; everything else is up to the image. The
# repository is not baked in: it arrives as a bind mount on /workspace, and the
# container runs as the host user so the files it writes stay owned by you.
# Likewise, code-server for the VS Code integration is never
# part of this image: Hexagon bind mounts one release for every session that
# asks for it, so it does not belong here either.

FROM node:22-bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      git tmux ca-certificates curl less ripgrep procps openssh-client \
    && rm -rf /var/lib/apt/lists/*

ENV SHELL=/bin/bash

RUN npm install -g @anthropic-ai/claude-code

# Claude Code updates itself, and the user it runs as is not known here: it is
# the host user. Hexagon adds gid 2000 to every container it runs Claude Code
# in, so handing the install to that group lets the updater replace it without
# making it writable by everyone. npm renames the old package away inside
# @anthropic-ai and swaps the bin symlink, so that tree needs it recursively and
# the two parent directories only for themselves: node and npm stay root's.
# /usr/local is npm's global prefix, and Claude Code decides whether it may
# auto-update by checking that the prefix itself is writable, although npm never
# writes there; without it the update only happens by hand. The update lives in
# the container, not the image: a recreated session starts from this.
RUN chgrp -R 2000 /usr/local/lib/node_modules/@anthropic-ai \
    && chmod -R g+w /usr/local/lib/node_modules/@anthropic-ai \
    && chgrp 2000 /usr/local /usr/local/lib/node_modules /usr/local/bin \
    && chmod g+w /usr/local /usr/local/lib/node_modules /usr/local/bin

# Git authenticates with the token Hexagon puts in the environment, so no
# credentials are ever written to disk inside the container.
RUN printf '#!/bin/sh\ncase "$1" in *Username*) echo x-access-token ;; *) echo "$GITHUB_TOKEN" ;; esac\n' \
      > /usr/local/bin/git-askpass && chmod +x /usr/local/bin/git-askpass
ENV GIT_ASKPASS=/usr/local/bin/git-askpass

WORKDIR /workspace
CMD ["sleep", "infinity"]
