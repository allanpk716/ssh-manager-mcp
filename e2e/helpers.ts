import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { mkdir, rm } from "node:fs/promises";
import * as https from "node:https";
import * as net from "node:net";
import os from "node:os";
import path from "node:path";
import { test as base, expect } from "@playwright/test";

const e2eDir = __dirname;
export const repoRoot = path.resolve(e2eDir, "..");
const artifactsDir = path.join(e2eDir, ".artifacts");
const binaryPath = path.join(artifactsDir, `sshmgr${process.platform === "win32" ? ".exe" : ""}`);
const testSSHServerPath = path.join(artifactsDir, `testsshd${process.platform === "win32" ? ".exe" : ""}`);

export type testSSHServer = {
  address: string;
  privateKeyPath: string;
  close: () => Promise<void>;
};

export type testSSHServerOptions = {
  handshakeDelayMilliseconds?: number;
};

let testSSHServerBuild: Promise<void> | undefined;

// buildTestSSHServer 每轮浏览器测试只编译一次 testsshd，之后直接启动这个
// 二进制而不是用 go run 代理启动：go run 收到 SIGTERM 不会把信号转发给
// 它编译出的子进程，测试结束后子进程会孤儿化常驻并继续监听端口（#318）。
// 直接启动后 close() 的信号命中服务本体，服务自身还监视标准输入管道关闭
// 自行退出，测试进程崩溃也能自清理。
function buildTestSSHServer(): Promise<void> {
  if (!testSSHServerBuild) {
    testSSHServerBuild = (async () => {
      await mkdir(artifactsDir, { recursive: true });
      await new Promise<void>((resolve, reject) => {
        const build = spawn("go", ["build", "-o", testSSHServerPath, "./e2e/tools/testsshd"], {
          cwd: repoRoot,
          stdio: ["pipe", "pipe", "pipe"],
        });
        let output = "";
        build.stdout.on("data", (chunk) => (output += chunk));
        build.stderr.on("data", (chunk) => (output += chunk));
        build.on("error", reject);
        build.on("close", (code) => (code === 0 ? resolve() : reject(new Error(`编译 testsshd 失败：${output}`))));
      });
    })();
  }
  return testSSHServerBuild;
}

// startTestSSHServer 编译并启动 e2e/tools/testsshd。它只监听本机随机端
// 口，并把配套私钥写入测试指定的临时路径。
export async function startTestSSHServer(
  privateKeyPath: string,
  options: testSSHServerOptions = {},
): Promise<testSSHServer> {
  await buildTestSSHServer();
  const server = spawn(testSSHServerPath, [privateKeyPath], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      ...(options.handshakeDelayMilliseconds === undefined
        ? {}
        : { SSHMGR_E2E_TEST_SSHD_DELAY_MS: String(options.handshakeDelayMilliseconds) }),
    },
  });
  // 关闭标准输入时进程若已退出，写端管道可能已坏；挂空错误处理，避免未
  // 处理的错误事件把测试进程打崩。
  server.stdin.on("error", () => {});
  let output = "";
  server.stdout.on("data", (chunk) => (output += chunk));
  server.stderr.on("data", (chunk) => (output += chunk));
  const address = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`等待 SSH 测试服务启动超时：\n${output}`)), 15_000);
    const poll = setInterval(() => {
      const match = output.match(/^127\.0\.0\.1:\d+$/m);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    server.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`SSH 测试服务提前退出（${code}）：\n${output}`));
    });
  });
  return {
    address,
    privateKeyPath,
    close: async () => {
      const exited = new Promise<void>((resolve) => {
        if (server.exitCode !== null) {
          resolve();
          return;
        }
        server.once("exit", () => resolve());
      });
      if (server.exitCode === null) {
        // 先关标准输入让服务自行关监听退出；两秒内没退再直接发 SIGTERM
        // 兜底，此时信号命中服务本体，不存在 go run 不转发的问题（#318）。
        server.stdin.end();
        const fallbackSignal = setTimeout(() => server.kill("SIGTERM"), 2_000);
        fallbackSignal.unref();
      }
      await exited;
    },
  };
}

export type silentSSHServer = {
  address: string;
  close: () => Promise<void>;
};

// startSilentSSHServer 启动只接受 TCP 连接但不完成 SSH 握手的本机服务，
// 供连接测试浏览器用例稳定触发超时。
export async function startSilentSSHServer(): Promise<silentSSHServer> {
  const connections = new Set<import("node:net").Socket>();
  const server = net.createServer((connection) => {
    connections.add(connection);
    connection.on("close", () => connections.delete(connection));
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === "string") {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    throw new Error("静默 SSH 测试服务没有返回监听地址");
  }
  return {
    address: `127.0.0.1:${address.port}`,
    close: async () => {
      for (const connection of connections) {
        connection.destroy();
      }
      await new Promise<void>((resolve, reject) => {
        server.close((error) => (error === undefined ? resolve() : reject(error)));
      });
    },
  };
}

// reserveFreePort 返回一个刚释放的本机端口。调用方应立即把它交给要监听
// 的测试服务或测试条目，避免其它进程抢先占用。
export async function reserveFreePort(): Promise<number> {
  const server = net.createServer();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === "string") {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    throw new Error("没有取得可释放的本机端口");
  }
  const port = address.port;
  await new Promise<void>((resolve, reject) => {
    server.close((error) => (error === undefined ? resolve() : reject(error)));
  });
  return port;
}

// goCacheEnvironment 保留本机 Go 构建缓存与工具链选择。种子进程把 HOME 指
// 到临时目录来隔离配置；不带上这几个变量时，go 会认为工具链缓存为空而重
// 新下载，客户端用例会慢到超时。
const goCacheValues = spawnSync("go", ["env", "GOCACHE", "GOMODCACHE", "GOPATH", "GOTOOLCHAIN"], {
  cwd: repoRoot,
  encoding: "utf8",
}).stdout.trim().split("\n");
const goCacheEnvironment = {
  GOCACHE: goCacheValues[0],
  GOMODCACHE: goCacheValues[1],
  GOPATH: goCacheValues[2],
  GOTOOLCHAIN: goCacheValues[3],
};

// seedVaultEnvironment 是 seedvault 种子进程的隔离环境：HOME 族与全部状态
// 路径显式指向测试临时目录（不依赖 SSHMGR_STORE 顺带钉住角色文件的间接
// 效应），Go 工具链环境保留本机值（HOME 改道后 go 仍复用缓存与工具链）。
function seedVaultEnvironment(runDir: string): Record<string, string> {
  return {
    ...process.env,
    ...goCacheEnvironment,
    HOME: runDir,
    APPDATA: runDir,
    XDG_CONFIG_HOME: runDir,
    SSHMGR_CONFIG_ROOT: runDir,
    SSHMGR_VAULT_ROOT: runDir,
    SSHMGR_STORE: path.join(runDir, "store.db"),
    SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
    SSHMGR_CACHE_DIR: path.join(runDir, "cache"),
  };
}

// buildBinary 在整轮浏览器测试开始前编译一次 sshmgr，测试进程全部用同一
// 个二进制，避免每个用例重复编译。
export async function buildBinary(): Promise<void> {
  await mkdir(artifactsDir, { recursive: true });
  await new Promise<void>((resolve, reject) => {
    const build = spawn("go", ["build", "-o", binaryPath, "./cmd/sshmgr"], {
      cwd: repoRoot,
      stdio: ["pipe", "pipe", "pipe"],
    });
    let output = "";
    build.stdout.on("data", (chunk) => (output += chunk));
    build.stderr.on("data", (chunk) => (output += chunk));
    build.on("error", reject);
    build.on("close", (code) => (code === 0 ? resolve() : reject(new Error(`go build 失败：${output}`))));
  });
}

// standaloneUI 描述一次已启动的单机 Web UI 进程。
export type standaloneUI = {
  startupURL: string;
  backupDir: string;
  passphraseFile: string;
  restart: () => Promise<string>;
  close: () => Promise<void>;
};

// clientUI 描述一次已启动的客户端 Web UI 进程。临时目录留给操作系统清理，
// 测试助手不做递归删除。
export type clientUI = {
  startupURL: string;
  close: () => Promise<void>;
};

// authorityUI 描述一次已启动的权威端 Web UI 进程。浏览器测试用临时加密库
// 与随机端口运行，不接触本机真实权威端数据。
export type authorityUI = {
  loginURL: string;
  password: string;
  close: () => Promise<void>;
};

// launchUIProcess 在给定临时目录里启动一次 `sshmgr ui`，并返回进程与一
// 次性启动链接。
// 子进程的 PATH 指向不存在的目录，这样「自动打开系统浏览器」会失败并只打
// 印链接，由 Playwright 自己使用这条一次性链接，避免真实浏览器抢先消耗它。
async function launchUIProcess(
  runDir: string,
  environment: Record<string, string> = {},
): Promise<{ process: ReturnType<typeof spawn>; startupURL: string }> {
  const backupDir = path.join(runDir, "backups");
  const passFile = path.join(runDir, "backup.pass");
  const ui = spawn(binaryPath, ["ui"], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
      PATH: path.join(runDir, "empty-path"),
      ...environment,
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));

  const startupURL = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待 Web UI 启动超时，输出：\n${output}`)),
      15_000,
    );
    const poll = setInterval(() => {
      const match = output.match(/https?:\/\/\S+/);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    ui.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`Web UI 进程提前退出（${code}），输出：\n${output}`));
    });
  });

  return {
    process: ui,
    startupURL,
    backupDir,
    passphraseFile: passFile,
  };
}

async function stopUIProcess(process: ReturnType<typeof spawn>): Promise<void> {
  process.kill("SIGTERM");
  await new Promise<void>((resolve) => {
    if (process.exitCode !== null) {
      resolve();
      return;
    }
    process.once("exit", () => resolve());
  });
}

// startUIProcess 启动可复用同一临时目录重启的单机 Web UI。restart 只停止
// 并重新拉起进程，close 才清理临时加密库。
async function startUIProcess(
  runDir: string,
  environment: Record<string, string> = {},
): Promise<standaloneUI> {
  let running = await launchUIProcess(runDir, environment);
  const ui: standaloneUI = {
    startupURL: running.startupURL,
    backupDir: running.backupDir,
    passphraseFile: running.passphraseFile,
    restart: async () => {
      await stopUIProcess(running.process);
      running = await launchUIProcess(runDir, environment);
      ui.startupURL = running.startupURL;
      return running.startupURL;
    },
    close: async () => {
      await stopUIProcess(running.process);
      await rm(runDir, { recursive: true, force: true });
    },
  };
  return ui;
}

// startStandaloneUI 为当前用例准备一个全新的临时加密库，以单机角色启动
// `sshmgr ui`，并返回一次性启动链接。
async function startStandaloneUI(setup?: (runDir: string) => void): Promise<standaloneUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-"));
  setup?.(runDir);
  const seed = spawnSync("go", ["run", "./e2e/tools/seedvault", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: seedVaultEnvironment(runDir),
  });
  if (seed.status !== 0) {
    await rm(runDir, { recursive: true, force: true });
    throw new Error(`准备临时加密库失败：${seed.stdout}\n${seed.stderr}`);
  }
  return startUIProcess(runDir);
}

// startEmptyMachineUI 在空临时目录上启动 `sshmgr ui`，用于浏览器里的初始
// 化向导测试。
async function startEmptyMachineUI(): Promise<standaloneUI> {
  return startUIProcess(mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-")));
}

// startStandaloneUIWithTopology 用带拓扑数据的种子库启动单机 Web UI，供网
// 络拓扑浏览器测试使用；其它用例的种子数据保持不变。probePort 提供时，种
// 子库给探测目标登记该端口，用例在本地监听它完成真实触发探测。
export async function startStandaloneUIWithTopology(
  probePort?: number,
  connectionTestPort?: number,
  environment?: Record<string, string>,
): Promise<standaloneUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-topology-"));
  const seedEnv: Record<string, string> = { SSHMGR_E2E_TOPOLOGY: "1" };
  if (probePort !== undefined) {
    seedEnv.SSHMGR_E2E_PROBE_PORT = String(probePort);
  }
  if (connectionTestPort !== undefined) {
    seedEnv.SSHMGR_E2E_CONNECTION_TEST_PORT = String(connectionTestPort);
  }
  const seed = spawnSync("go", ["run", "./e2e/tools/seedvault", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...seedVaultEnvironment(runDir),
      ...seedEnv,
    },
  });
  if (seed.status !== 0) {
    await rm(runDir, { recursive: true, force: true });
    throw new Error(`准备拓扑临时加密库失败：${seed.stdout}\n${seed.stderr}`);
  }
  return startUIProcess(runDir, environment);
}

// startClientUIWithTopology 用带拓扑数据的本地快照启动客户端 Web UI，供网
// 络拓扑浏览器测试验证只读视图与按快照裁剪的呈现。
export async function startClientUIWithTopology(): Promise<clientUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-topology-client-"));
  const seed = spawnSync("go", ["run", "./e2e/tools/seedclient", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      ...goCacheEnvironment,
      SSHMGR_E2E_TOPOLOGY: "1",
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
    },
  });
  if (seed.status !== 0) {
    throw new Error(`准备拓扑客户端缓存失败：${seed.stdout}\n${seed.stderr}`);
  }
  const ui = spawn(binaryPath, ["ui"], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_CACHE_DIR: path.join(runDir, "cache"),
      SSHMGR_CACHE_DEK: path.join(runDir, "cache-dek.key"),
      PATH: path.join(runDir, "empty-path"),
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));
  const startupURL = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待拓扑客户端 Web UI 启动超时，输出：\n${output}`)),
      15_000,
    );
    const poll = setInterval(() => {
      const match = output.match(/https?:\/\/\S+/);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    ui.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`拓扑客户端 Web UI 进程提前退出（${code}），输出：\n${output}`));
    });
  });
  return {
    startupURL,
    close: async () => {
      ui.kill("SIGTERM");
      await new Promise<void>((resolve) => {
        if (ui.exitCode !== null) {
          resolve();
          return;
        }
        ui.on("exit", () => resolve());
      });
    },
  };
}

// startClientUI 为当前用例生成一个客户端本地快照缓存，并以客户端角色启
// 动 `sshmgr ui`。PATH 指向不存在目录，避免系统浏览器抢先消耗启动链接。
async function startClientUI(): Promise<clientUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-client-"));
  const seed = spawnSync("go", ["run", "./e2e/tools/seedclient", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      ...goCacheEnvironment,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
    },
  });
  if (seed.status !== 0) {
    throw new Error(`准备客户端缓存失败：${seed.stdout}\n${seed.stderr}`);
  }

  const ui = spawn(binaryPath, ["ui"], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_CACHE_DIR: path.join(runDir, "cache"),
      SSHMGR_CACHE_DEK: path.join(runDir, "cache-dek.key"),
      PATH: path.join(runDir, "empty-path"),
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));

  const startupURL = await new Promise<string>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待客户端 Web UI 启动超时，输出：\n${output}`)),
      15_000,
    );
    const poll = setInterval(() => {
      const match = output.match(/https?:\/\/\S+/);
      if (match) {
        clearTimeout(timer);
        clearInterval(poll);
        resolve(match[0]);
      }
    }, 50);
    ui.on("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`客户端 Web UI 进程提前退出（${code}），输出：\n${output}`));
    });
  });

  return {
    startupURL,
    close: async () => {
      ui.kill("SIGTERM");
      await new Promise<void>((resolve) => {
        if (ui.exitCode !== null) {
          resolve();
          return;
        }
        ui.on("exit", () => resolve());
      });
    },
  };
}

function reserveLocalPort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const listener = net.createServer();
    listener.once("error", reject);
    listener.listen(0, "127.0.0.1", () => {
      const address = listener.address();
      if (typeof address !== "object" || address === null) {
        listener.close(() => reject(new Error("无法取得临时监听端口")));
        return;
      }
      const port = address.port;
      listener.close(() => resolve(port));
    });
  });
}

// startAuthorityUI 生成角色为权威端的临时加密库，设置管理员口令与随机
// Web UI 端口，再启动 serve。发现服务显式关闭，测试不占用本机的 UDP 7878。
async function startAuthorityUI(password: string, pairingMode = "", clientsSeed = false): Promise<authorityUI> {
  const runDir = mkdtempSync(path.join(os.tmpdir(), "sshmgr-e2e-authority-"));
  const servePort = await reserveLocalPort();
  const webPort = await reserveLocalPort();
  const seed = spawnSync("go", ["run", "./e2e/tools/seedvault", runDir], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...seedVaultEnvironment(runDir),
      SSHMGR_E2E_ROLE: "server",
      SSHMGR_E2E_WEB_PORT: String(webPort),
      SSHMGR_E2E_ADMIN_PASSWORD: password,
      SSHMGR_E2E_CLIENTS: clientsSeed ? "1" : "",
    },
  });
  if (seed.status !== 0) {
    throw new Error(`准备权威端临时加密库失败：${seed.stdout}\n${seed.stderr}`);
  }
  const ui = spawn(binaryPath, [
    "serve", "--addr", `127.0.0.1:${servePort}`, "--discovery=false",
    "--tls-cert", path.join(runDir, "browser-test-cert.pem"),
    "--tls-key", path.join(runDir, "browser-test-key.pem"),
  ], {
    cwd: repoRoot,
    stdio: ["pipe", "pipe", "pipe"],
    env: {
      ...process.env,
      HOME: runDir,
      APPDATA: runDir,
      XDG_CONFIG_HOME: runDir,
      SSHMGR_CONFIG_ROOT: runDir,
      SSHMGR_VAULT_ROOT: runDir,
      SSHMGR_STORE: path.join(runDir, "store.db"),
      SSHMGR_FILEKEY_PATH: path.join(runDir, "master.key.plain"),
      SSHMGR_E2E_REVERIFY_WINDOW: "1s",
    },
  });
  let output = "";
  ui.stdout.on("data", (chunk) => (output += chunk));
  ui.stderr.on("data", (chunk) => (output += chunk));
  const loginURL = `https://127.0.0.1:${webPort}/ui/`;
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error(`等待权威端 Web UI 超时，输出：\n${output}`)),
      20_000,
    );
    const tryConnect = () => {
      const request = https.get(loginURL, { rejectUnauthorized: false }, (response) => {
        response.resume();
        clearTimeout(timer);
        request.destroy();
        resolve();
      });
      request.once("error", () => request.destroy());
    };
    const poll = setInterval(tryConnect, 100);
    ui.once("exit", (code) => {
      clearTimeout(timer);
      clearInterval(poll);
      reject(new Error(`权威端 Web UI 提前退出（${code}），输出：\n${output}`));
    });
    tryConnect();
  });
  if (pairingMode !== "") {
    const seedPairing = spawnSync("go", ["run", "./e2e/tools/seedpairing", runDir], {
      cwd: repoRoot,
      encoding: "utf8",
      env: { ...process.env, SSHMGR_E2E_PAIRING: pairingMode },
    });
    if (seedPairing.status !== 0) {
      throw new Error(`准备配对请求失败：${seedPairing.stdout}\n${seedPairing.stderr}`);
    }
  }
  return {
    loginURL,
    password,
    close: async () => {
      ui.kill("SIGTERM");
      await new Promise<void>((resolve) => {
        if (ui.exitCode !== null) {
          resolve();
          return;
        }
        ui.on("exit", () => resolve());
      });
    },
  };
}

// standaloneUIFixture 让每个用例拿到自己的 Web UI 进程；用例结束（无论成
// 败）都会停止进程并删除临时加密库。
type standaloneUIFixture = { standaloneUI: standaloneUI };
// sharesStandaloneUIFixture 给共享存储用例额外种入挂载密码与登记数据。
type sharesStandaloneUIFixture = { sharesStandaloneUI: standaloneUI };
type clientUIFixture = { clientUI: clientUI };
type authorityUIFixture = { authorityUI: authorityUI };

export const test = base.extend<
  standaloneUIFixture & sharesStandaloneUIFixture & clientUIFixture & authorityUIFixture
>({
  standaloneUI: async ({}, use) => {
    const ui = await startStandaloneUI();
    await use(ui);
    await ui.close();
  },
  sharesStandaloneUI: async ({}, use) => {
    process.env.SSHMGR_E2E_SHARES = "1";
    try {
      const ui = await startStandaloneUI();
      await use(ui);
      await ui.close();
    } finally {
      delete process.env.SSHMGR_E2E_SHARES;
    }
  },
  clientUI: async ({}, use) => {
    const ui = await startClientUI();
    await use(ui);
    await ui.close();
  },
  authorityUI: async ({}, use) => {
    const ui = await startAuthorityUI("browser-test-password");
    await use(ui);
    await ui.close();
  },
});

export { expect, startAuthorityUI, startClientUI, startStandaloneUI, startEmptyMachineUI };

// gotoUIPath 以当前页面地址为基准打开同一个 Web UI 里的路径；测试配置不设
// baseURL，直接 page.goto("/ui/...") 无法解析相对地址。
export async function gotoUIPath(
  page: import("@playwright/test").Page,
  path: string,
): Promise<void> {
  await page.goto(new URL(path, page.url()).toString());
}

// openStandaloneUI 用一次性启动链接进入 Web UI 主页，再切到服务器列表；
// 大多数用例的起点仍是服务器列表。
export async function openStandaloneUI(
  page: import("@playwright/test").Page,
  ui: standaloneUI,
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "服务器" }).click();
  await page.getByRole("heading", { name: /^服务器/ }).first().waitFor();
}

// openClientUI 用一次性启动链接进入客户端 Web UI 主页，再切到服务器列表。
export async function openClientUI(
  page: import("@playwright/test").Page,
  ui: clientUI,
): Promise<void> {
  await page.goto(ui.startupURL);
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
  await page.getByRole("navigation", { name: "一级导航" }).getByRole("link", { name: "服务器" }).click();
  await page.getByRole("heading", { name: /^服务器/ }).first().waitFor();
}

// openAuthorityUI 进入权威端登录页并完成管理员口令登录。
export async function openAuthorityUI(
  page: import("@playwright/test").Page,
  ui: authorityUI,
  password: string,
): Promise<void> {
  await page.goto(ui.loginURL);
  await page.getByLabel("管理员口令").fill(password);
  await page.getByRole("button", { name: "登录" }).click();
  await page.getByRole("heading", { name: "状态总览" }).waitFor();
}
