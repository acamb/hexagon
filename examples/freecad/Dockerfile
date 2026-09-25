# Hexagon FreeCAD image: FreeCAD in the browser (noVNC) beside Claude Code,
# which drives it through the freecad-mcp addon.
#
# Built on lscr.io/linuxserver/freecad for its FreeCAD build, not for its init:
# the s6 overlay needs root, and Hexagon runs a session container as the host
# user. The entrypoint below starts Xvnc, openbox, noVNC and FreeCAD as that
# user instead, then runs Hexagon's own command.
#
# Publish port 3000 on the session to see FreeCAD. It has no password unless
# ~/.vnc/passwd exists (run `vncpasswd` in the session, then restart it), so
# bind it to 127.0.0.1 unless the machine is on a network you trust.
#
# Hexagon builds with the legacy builder and a context holding nothing but this
# file, so there is no COPY: the scripts are written inline. This file is
# generated from Dockerfile.in and src/ by `make dockerfile`; edit those.

# Node.js, for npm: the same image Hexagon's own reference base image builds
# on, copied whole rather than reinstalled, since that image's /usr/local
# holds nothing but this Node.js release.
FROM node:22-bookworm-slim AS node

FROM lscr.io/linuxserver/freecad:latest

# freecad-mcp, pinned: both the FreeCAD addon and the MCP server come from this
# commit, so the two halves of the RPC protocol always match.
ARG FREECAD_MCP_REF=751974609a401660a58a1772ef16f3afbeba9ba1

RUN apt-get update && apt-get install -y --no-install-recommends \
      tmux ripgrep less openssh-client procps tini \
      tigervnc-standalone-server tigervnc-tools novnc websockify \
    && rm -rf /var/lib/apt/lists/*

COPY --from=node /usr/local /usr/local

# Claude Code, system-wide via npm, not the native installer: the session's
# HOME is a bind mount, so anything the native installer keeps under ~/.local
# would vanish, and npm's global install can update itself the way the grant
# below expects.
RUN npm install -g @anthropic-ai/claude-code

# Claude Code updates itself, and the user it runs as is not known here: it is
# the host user. Hexagon adds gid 2000 to every container it runs Claude Code
# in, so handing the install to that group lets the updater replace it without
# making it writable by everyone. Mirrors deploy/images/base/Dockerfile in
# Hexagon's own repository exactly, since npm's prefix here is also
# /usr/local: see that file for the full reasoning.
RUN chgrp -R 2000 /usr/local/lib/node_modules/@anthropic-ai \
    && chmod -R g+w /usr/local/lib/node_modules/@anthropic-ai \
    && chgrp 2000 /usr/local /usr/local/lib/node_modules /usr/local/bin \
    && chmod g+w /usr/local /usr/local/lib/node_modules /usr/local/bin

# uv, and the freecad-mcp server installed with its own Python under /opt.
RUN curl -LsSf https://astral.sh/uv/install.sh \
      | env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh \
    && git init -q /tmp/freecad-mcp \
    && git -C /tmp/freecad-mcp fetch -q --depth 1 https://github.com/neka-nat/freecad-mcp "$FREECAD_MCP_REF" \
    && git -C /tmp/freecad-mcp checkout -q FETCH_HEAD \
    && UV_TOOL_DIR=/opt/uv/tools UV_TOOL_BIN_DIR=/usr/local/bin UV_PYTHON_INSTALL_DIR=/opt/uv/python \
       UV_CACHE_DIR=/tmp/uv-cache uv tool install --managed-python --python 3.12 /tmp/freecad-mcp \
    && cp -r /tmp/freecad-mcp/addon/FreeCADMCP /opt/freecad/usr/Mod/FreeCADMCP \
    && sed -i 's/"auto_start_rpc": False/"auto_start_rpc": True/' \
         /opt/freecad/usr/Mod/FreeCADMCP/rpc_server/settings.py \
    && grep -q '"auto_start_rpc": True' /opt/freecad/usr/Mod/FreeCADMCP/rpc_server/settings.py \
    && rm -rf /tmp/freecad-mcp /tmp/uv-cache

# freecad-mcp's execute_code_headless looks for freecadcmd on the PATH.
RUN printf '#!/bin/sh\nexec /opt/freecad/AppRun freecadcmd "$@"\n' > /usr/local/bin/freecadcmd \
    && chmod 0755 /usr/local/bin/freecadcmd

# noVNC, opening straight into the desktop and resizing it to the browser tab.
RUN mkdir -p /opt/hexagon-freecad \
    && cp -r /usr/share/novnc /opt/hexagon-freecad/novnc \
    && printf '%s\n' '<!doctype html><meta http-equiv="refresh" content="0; url=vnc.html?autoconnect=1&resize=remote&reconnect=1">' \
         > /opt/hexagon-freecad/novnc/index.html

# FreeCAD fills the screen, without a title bar.
RUN sed -i 's|<applications>|<applications><application class="FreeCAD"><decor>no</decor><maximized>yes</maximized></application>|' \
      /etc/xdg/openbox/rc.xml

# src/hexagon-freecad-mcp-register.py
RUN printf '%s\n' \
      '#!/usr/bin/env python3' \
      '"""Register the FreeCAD MCP server in ~/.claude.json, user scope.' \
      '' \
      'Hexagon bind mounts the session'"'"'s HOME over the image'"'"'s, so nothing written to' \
      'HOME at build time survives: this runs at every container start instead. It' \
      'only adds the entry when there is no "freecad" server, so one edited by hand' \
      'is kept as it is; one removed comes back at the next start.' \
      '' \
      'The file is replaced atomically, and left alone if it cannot be parsed: a' \
      'corrupt file is Claude Code'"'"'s to report, not ours to overwrite.' \
      '"""' \
      '' \
      'import json' \
      'import os' \
      'import sys' \
      'import tempfile' \
      '' \
      'SERVER = {"type": "stdio", "command": "freecad-mcp", "args": [], "env": {}}' \
      '' \
      '' \
      'def main() -> int:' \
      '    path = os.path.join(os.path.expanduser("~"), ".claude.json")' \
      '    try:' \
      '        with open(path) as f:' \
      '            data = json.load(f)' \
      '    except FileNotFoundError:' \
      '        data = {}' \
      '    except (OSError, ValueError) as e:' \
      '        print(f"{path}: {e}; not touching it", file=sys.stderr)' \
      '        return 1' \
      '' \
      '    servers = data.setdefault("mcpServers", {})' \
      '    if "freecad" in servers:' \
      '        return 0' \
      '    servers["freecad"] = SERVER' \
      '' \
      '    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".claude.json.")' \
      '    try:' \
      '        with os.fdopen(fd, "w") as f:' \
      '            json.dump(data, f, indent=2)' \
      '        os.chmod(tmp, 0o600)' \
      '        os.replace(tmp, path)' \
      '    except BaseException:' \
      '        os.unlink(tmp)' \
      '        raise' \
      '    return 0' \
      '' \
      '' \
      'if __name__ == "__main__":' \
      '    sys.exit(main())' \
    > /usr/local/bin/hexagon-freecad-mcp-register && chmod 0755 /usr/local/bin/hexagon-freecad-mcp-register
# src/hexagon-freecad-entry.sh
RUN printf '%s\n' \
      '#!/bin/bash' \
      '# Entrypoint of the Hexagon FreeCAD image.' \
      '#' \
      '# Hexagon starts a session container as the host user and replaces CMD with' \
      '# `sleep infinity`, but leaves ENTRYPOINT alone. So this script is what brings' \
      '# the desktop up: it starts it as whoever the container runs as, in the' \
      '# background, and then hands PID 1'"'"'s job to the command it was given. The' \
      '# linuxserver.io s6 init is never used: it has to start as root, and a session' \
      '# container never is.' \
      '#' \
      '# What runs, all of it bound to loopback except noVNC:' \
      '#   Xvnc      :1, VNC on 127.0.0.1:5901' \
      '#   openbox   window manager, keeps FreeCAD maximized' \
      '#   websockify/noVNC on 0.0.0.0:$NOVNC_PORT (3000), the port to publish' \
      '#   FreeCAD   restarted whenever it exits, with the MCP addon'"'"'s RPC server' \
      '#             listening on 127.0.0.1:9875' \
      '#' \
      '# Logs go to /tmp/hexagon-freecad/*.log.' \
      '' \
      'set -u' \
      '' \
      'LOGS=/tmp/hexagon-freecad' \
      'mkdir -p "$LOGS"' \
      '' \
      'export DISPLAY="${DISPLAY:-:1}"' \
      'export HOME="${HOME:-/tmp/home}"' \
      'mkdir -p "$HOME"' \
      'display_number="${DISPLAY#:}"' \
      '' \
      'log() { printf '"'"'[hexagon-freecad] %s\n'"'"' "$*" >&2; }' \
      '' \
      '# A uid that is not in /etc/passwd works for almost everything, but some' \
      '# programs (ssh, and Qt'"'"'s user lookups) want a name. The image leaves' \
      '# /etc/passwd writable for exactly this line.' \
      'if ! getent passwd "$(id -u)" >/dev/null && [ -w /etc/passwd ]; then' \
      '  echo "agent:x:$(id -u):$(id -g):agent:$HOME:/bin/bash" >>/etc/passwd' \
      'fi' \
      '' \
      '# First, before anything that takes time: Hexagon starts claude in tmux right' \
      '# after the container starts, and the MCP server has to be in ~/.claude.json' \
      '# before claude reads it.' \
      'hexagon-freecad-mcp-register || log "could not register the FreeCAD MCP server"' \
      '' \
      '# A stale lock from a container that was killed rather than stopped would make' \
      '# Xvnc refuse the display.' \
      'rm -f "/tmp/.X${display_number}-lock" "/tmp/.X11-unix/X${display_number}"' \
      '' \
      '# No VNC password unless one exists: create it inside the session with' \
      '# `vncpasswd` and restart the session to require it.' \
      'vnc_auth=(-SecurityTypes None)' \
      'if [ -s "$HOME/.vnc/passwd" ]; then' \
      '  vnc_auth=(-SecurityTypes VncAuth -rfbauth "$HOME/.vnc/passwd")' \
      '  log "VNC password required (from ~/.vnc/passwd)"' \
      'fi' \
      '' \
      'Xvnc "$DISPLAY" \' \
      '  -geometry "${FREECAD_GEOMETRY:-1920x1080}" -depth 24 \' \
      '  -localhost -rfbport 5901 "${vnc_auth[@]}" \' \
      '  -AlwaysShared -AcceptSetDesktopSize -SendCutText -AcceptCutText \' \
      '  >"$LOGS/xvnc.log" 2>&1 &' \
      '' \
      'for _ in $(seq 50); do' \
      '  [ -S "/tmp/.X11-unix/X${display_number}" ] && break' \
      '  sleep 0.1' \
      'done' \
      '' \
      'openbox >"$LOGS/openbox.log" 2>&1 &' \
      '' \
      'websockify --web /opt/hexagon-freecad/novnc "${NOVNC_PORT:-3000}" 127.0.0.1:5901 \' \
      '  >"$LOGS/novnc.log" 2>&1 &' \
      '' \
      '(' \
      '  while true; do' \
      '    /opt/freecad/AppRun >>"$LOGS/freecad.log" 2>&1' \
      '    log "FreeCAD exited, restarting in 2s"' \
      '    sleep 2' \
      '  done' \
      ') &' \
      '' \
      'exec "$@"' \
    > /usr/local/bin/hexagon-freecad-entry && chmod 0755 /usr/local/bin/hexagon-freecad-entry

# The container's uid is not known until it starts; the entrypoint adds it.
RUN chmod 0666 /etc/passwd

# Hexagon replaces CMD and HOME, and needs git, tmux and claude on the PATH.
ENV SHELL=/bin/bash \
    HOME=/home/agent \
    NOVNC_PORT=3000
EXPOSE 3000
WORKDIR /workspace
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/hexagon-freecad-entry"]
CMD ["sleep", "infinity"]