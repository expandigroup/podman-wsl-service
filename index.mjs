import fs from 'fs';
import http from 'http';
import net from 'net';
import path from 'path';
import systemdSocket from 'systemd-socket';
import url from 'url';
import { execFileSync } from 'child_process';
import { program } from 'commander';

const defaultUpstreamSocketPath = '/mnt/wsl/podman-sockets/podman-machine-default/podman-root.sock';
const defaultDownstreamSocketPath = '/run/podman/podman.sock';

program
  .name('podman-wsl-service')
  .option('-l, --log-level <level>', 'Set the log level (debug, info, error)', 'info')
  .option('-u, --upstream-socket <path>', 'The path to the upstream podman socket', defaultUpstreamSocketPath)
  .option('-d, --downstream-socket <path>', 'The path to the downstream podman socket', defaultDownstreamSocketPath)
  .option('-n, --wsl-distro-name <name>', 'The name of the WSL distro (default: autodetect)', '')
  .option('-M, --no-mount-distro-root', 'Do not mount the distro root')
  .option(
    '-t, --shutdown-timeout <timeout>',
    'Time in seconds after which the proxy will shut down if no connections are active (-1 to disable, default: disabled)',
    '-1'
  )
  .parse(process.argv);

const options = program.opts();
const logLevel = options.logLevel;
const upstreamSocketPath = options.upstreamSocket;
const downstreamSocketPath = options.downstreamSocket;
const wslDistroName = options.wslDistroName;
const mountDistroRoot = options.mountDistroRoot;
const shutdownTimeout = parseInt(options.shutdownTimeout);

const sharedRoot = getSharedMountpoint(wslDistroName || getWslDistroName());

const systemdSocketFd = systemdSocket();

const logLevels = ['debug', 'info', 'error'];
if (!logLevels.includes(logLevel)) {
  console.error(`Invalid log level: ${logLevel}`);
  process.exit(1);
}
console.log = (msg) => (logLevel === 'info' || logLevel === 'debug' ? console.info(msg) : () => {
});
console.debug = (msg) => (logLevel === 'debug' ? console.info(msg) : () => {
});

console.debug('Options:');
console.debug(`- Log level: ${logLevel}`);
console.debug(`- Upstream socket: ${upstreamSocketPath}`);
console.debug(`- Downstream socket: ${systemdSocketFd && systemdSocketFd.fd ? 'systemd' : downstreamSocketPath}`);
console.debug(`- WSL distro name: ${wslDistroName || 'autodetect'}`);
console.debug(`- Mount distro root: ${!mountDistroRoot}`);
console.debug(`- Shutdown timeout: ${shutdownTimeout < 0 ? 'disabled' : `${shutdownTimeout} seconds`}`);
console.debug(`- Shared root: ${sharedRoot}`);

if (mountDistroRoot) {
  console.log(`Mounting shared mountpoint: ${sharedRoot}`);
  mountSharedMountpoint(sharedRoot);
}

let activeConnections = 0;
let shutdownTimer = null;

function resetShutdownTimer() {
  if (shutdownTimer) {
    clearTimeout(shutdownTimer);
    shutdownTimer = null;
  }
  if (shutdownTimeout > 0) {
    shutdownTimer = setTimeout(() => {
      console.log('No active connections, shutting down.');
      cleanup();
    }, shutdownTimeout * 1000);
  }
}

function getWslDistroName() {
  const distroName = process.env.WSL_DISTRO_NAME;
  if (distroName) {
    return distroName;
  }
  try {
    const result = execFileSync('wslpath', ['-am', '/']).toString().trim();
    const parts = result.split('/').filter(Boolean);
    if (parts.length !== 2) {
      // noinspection ExceptionCaughtLocallyJS
      throw new Error('Unexpected value from "wslpath -am /"');
    }
    return parts[1];
  } catch (err) {
    console.error(`Unable to get the WSL distro name: ${err.message}`);
    process.exit(1);
  }
}

function getSharedMountpoint(distroName) {
  return `/mnt/wsl/distro-roots/${distroName}`;
}

function mountSharedMountpoint(mountPoint) {
  if (!fs.existsSync(mountPoint)) {
    fs.mkdirSync(mountPoint, { recursive: true });
  }
  try {
    // Check if the mount point is already mounted
    let isMounted;
    try {
      execFileSync('mountpoint', ['-q', mountPoint]);
      isMounted = true;
    } catch (err) {
      isMounted = false;
    }

    let options = 'rbind,rslave';
    if (isMounted) {
      options += ',remount';
    }
    execFileSync('mount', ['--make-shared', '/']);
    execFileSync('mount', ['/', mountPoint, '-o', options]);
  } catch (err) {
    console.error(`Unable to mount the shared mountpoint: ${err.message}`);
    process.exit(1);
  }
}

function writeError(res, statusCode, message, err) {
  res.writeHead(statusCode, { 'Content-Type': 'application/json' });
  res.end(
    JSON.stringify({
      response: statusCode,
      message: `podman-wsl-service: ${message}: ${err.message}`,
      cause: err.message
    })
  );
}

function wslPathToWindowsPath(wslPath) {
  return execFileSync('wslpath', ['-aw', wslPath]).toString().trim();
}

function translateHostPath(hostPath) {
  if (hostPath.startsWith('/mnt/wsl/')) {
    return hostPath;
  }

  try {
    const winPath = wslPathToWindowsPath(hostPath);
    if (winPath.startsWith('\\\\wsl.localhost\\')) {
      if (!hostPath.startsWith('/')) {
        // noinspection ExceptionCaughtLocallyJS
        throw new Error(`PODMAN WSL SERVICE BUG: unexpected path format, expected absolute path: '${hostPath}'`);
      }
      return path.join(sharedRoot, hostPath.slice(1));
    }
    return winPath;
  } catch (err) {
    console.error('Error translating host path:', err);
    throw err;
  }
}

function patchVolumesLibpod(body) {
  const mounts = body.mounts;
  if (!Array.isArray(mounts)) {
    return;
  }

  for (let i = 0; i < mounts.length; i++) {
    const mount = mounts[i];
    const hostPath = mount.source;
    try {
      mounts[i].source = translateHostPath(hostPath);
    } catch (err) {
      console.error('Error mangling volumes (libpod):', err);
      throw err;
    }
  }
}

function patchVolumesDocker(body) {
  const mounts = body.HostConfig?.Binds;
  if (!Array.isArray(mounts)) {
    return;
  }

  for (let i = 0; i < mounts.length; i++) {
    const mount = mounts[i].split(':');
    const hostPath = mount[0];
    try {
      mount[0] = translateHostPath(hostPath);
      mounts[i] = mount.join(':');
    } catch (err) {
      console.error('Error mangling volumes (docker):', err);
      throw err;
    }
  }
}

async function forwardRequest(req, res, modifiedBody = null) {
  console.log(`${res.statusCode} ${req.method} ${req.url} - intercepted: ${modifiedBody === null ? 'no' : 'yes'}`);
  const headers = { ...req.headers };

  const options = {
    socketPath: upstreamSocketPath,
    method: req.method,
    headers,
    path: req.url
  };

  if (modifiedBody) {
    options.headers['Content-Length'] = Buffer.byteLength(modifiedBody);
  }

  const upstreamReq = http.request(options, (upstreamRes) => {
    // Set response headers, preserving capitalization
    upstreamRes.rawHeaders.forEach((value, index) => {
      if (index % 2 === 0) {
        const headerName = value;
        const headerValue = upstreamRes.rawHeaders[index + 1];
        res.setHeader(headerName, headerValue);
      }
    }); // Write the status code and flush headers immediately

    res.writeHead(upstreamRes.statusCode);
    res.flushHeaders(); // Handle data manually

    upstreamRes.on('data', (chunk) => {
      const writeSuccess = res.write(chunk);
      if (!writeSuccess) {
        upstreamRes.pause();
      }
    });

    res.on('drain', () => {
      upstreamRes.resume();
    });

    upstreamRes.on('end', () => {
      res.end();
    });

    upstreamRes.on('error', (err) => {
      console.error(`Error in upstream response: ${err.message}`);
      res.end();
    });

    res.on('close', () => {
      upstreamRes.destroy();
    });

    res.on('error', (err) => {
      console.error(`Error in response: ${err.message}`);
      upstreamRes.destroy();
    });
  });

  upstreamReq.on('error', (err) => {
    console.error(`Error proxying request: ${err.message}`);
    if (!res.headersSent) {
      writeError(res, 500, 'Error proxying request', err);
    } else {
      res.end('Internal server error');
    }
  });

  if (modifiedBody) {
    upstreamReq.write(modifiedBody);
    upstreamReq.end();
  } else if (parseInt(req.headers['content-length'] || '0') > 0 || req.headers['transfer-encoding']) {
    // If the request has a body, pipe it
    req.pipe(upstreamReq);
  } else {
    // If no body, just end the upstream request
    upstreamReq.end();
  } // Handle client request errors

  req.on('aborted', () => {
    console.log(`Client request aborted: ${req.url}`);
    upstreamReq.abort();
  });

  req.on('error', (err) => {
    console.error(`Error in client request: ${err.message}`);
    upstreamReq.abort();
  });
}

// Create an HTTP server that listens on a Unix socket
const server = http.createServer(async (req, res) => {
  activeConnections++;
  if (shutdownTimer) {
    clearTimeout(shutdownTimer);
    shutdownTimer = null;
  }

  res.on('finish', () => {
    activeConnections--;
    if (activeConnections === 0) {
      resetShutdownTimer();
    }
  });

  const parsedUrl = url.parse(req.url);
  const pathWithoutVersion = parsedUrl.pathname.replace(/^\/v\d+\.(?:\d\.?)+\//, '/');

  if (
    req.method === 'POST' &&
    (pathWithoutVersion === '/containers/create' || pathWithoutVersion === '/libpod/containers/create')
  ) {
    let body = '';
    req.on('data', (chunk) => {
      body += chunk;
    });
    req.on('end', async () => {
      try {
        const jsonBody = JSON.parse(body);
        if (pathWithoutVersion === '/containers/create') {
          patchVolumesDocker(jsonBody);
        } else if (pathWithoutVersion === '/libpod/containers/create') {
          patchVolumesLibpod(jsonBody);
        }
        await forwardRequest(req, res, JSON.stringify(jsonBody));
      } catch (err) {
        console.error('Error processing request body:', err);
        writeError(res, 500, 'Error processing request body', err);
      }
    });
  } else {
    await forwardRequest(req, res);
  }
});

server.on('upgrade', (req, socket, head) => {
  activeConnections++;
  if (shutdownTimer) {
    clearTimeout(shutdownTimer);
    shutdownTimer = null;
  }

  socket.on('close', () => {
    activeConnections--;
    if (activeConnections === 0) {
      resetShutdownTimer();
    }
  });

  console.log(`101 ${req.method} ${req.url} - WebSocket upgrade`);
  const upstreamSocket = net.connect(upstreamSocketPath, () => {
    let headers = `${req.method} ${req.url} HTTP/${req.httpVersion}\r\n`;
    for (let i = 0; i < req.rawHeaders.length; i += 2) {
      headers += `${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}\r\n`;
    }
    headers += '\r\n';
    upstreamSocket.write(headers);
    upstreamSocket.write(head);
    socket.pipe(upstreamSocket).pipe(socket);
  });

  upstreamSocket.on('error', (err) => {
    console.error(`    WebSocket error: ${err.message} - ${req.method} ${req.url}`);
    socket.destroy();
  });

  socket.on('error', (err) => {
    console.error(`    Client WebSocket error: ${err.message} - ${req.method} ${req.url}`);
    upstreamSocket.destroy();
  });

  socket.on('close', () => {
    console.debug(`    Client WebSocket disconnected - ${req.method} ${req.url}`);
    upstreamSocket.destroy();
  });

  upstreamSocket.on('close', () => {
    console.debug(`    Upstream WebSocket disconnected - ${req.method} ${req.url}`);
    socket.destroy();
  });
});

function cleanup() {
  console.log('Cleaning up and closing Unix socket.');
  if (!systemdSocketFd && fs.existsSync(downstreamSocketPath)) {
    fs.unlinkSync(downstreamSocketPath);
    console.log('Closed Unix socket.');
  }
  process.exit();
}

process.on('SIGINT', cleanup);
process.on('SIGTERM', cleanup);

// Listen on a Unix socket
server.listen(systemdSocketFd || downstreamSocketPath, () => {
  console.log('Proxy server is listening on Unix socket');
  resetShutdownTimer();
});
