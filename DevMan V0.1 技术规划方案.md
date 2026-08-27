# DevMan V0.1 技术规划方案

> 暂定项目名：**DevMan**
>
> 产品定义：**AI-native Local Development Runtime Manager**
>
> 用一句话描述：  
> 让开发者和 AI 通过一个统一的桌面控制平面，管理本机所有 AI Coding 项目的前端、后端、Worker、AI Runtime、Docker 服务、端口、日志和健康状态。

---

# 1. V0.1 产品目标

DevMan V0.1 必须真正解决以下问题：

1. 一个项目包含 frontend、backend、AI worker、scheduler、Docker 等多个服务，需要分别启动。
2. 多项目同时开发时 Terminal 数量失控。
3. 不同项目容易产生端口冲突。
4. AI Agent 经常自行启动长期进程，导致沙箱、Terminal、PID、日志混乱。
5. AI 不知道一个项目“应该怎么启动”。
6. AI 修改代码后，不知道应该重启哪个服务。
7. 开发者不知道某个项目目前有哪些服务正在运行。
8. 项目关闭后容易遗留孤儿进程。
9. Windows / macOS / Linux 的 shell 和启动机制不同。
10. 项目换电脑或重新 clone 后，需要重新回忆启动命令。

DevMan V0.1 的目标：

> **让“项目运行方式”成为项目本身的一部分，而不再存在于开发者记忆、README 或若干 Terminal 中。**

---

# 2. V0.1 核心原则

整个项目必须遵循以下架构原则。

## 2.1 DevMan 拥有进程生命周期

禁止：

```text
Codex
   ↓
shell
   ↓
pnpm dev
```

长期运行。

应该：

```text
Codex
   ↓
DevMan CLI / API
   ↓
DevMan Daemon
   ↓
pnpm dev
```

即：

> **AI 不拥有长期服务进程，DevMan Daemon 拥有。**

---

## 2.2 GUI 不是核心，Daemon 才是核心

必须设计为：

```text
                 ┌──────────── GUI
                 │
                 ├──────────── CLI
                 │
                 ├──────────── Codex Skill
                 │
                 └──────────── Future MCP
                              │
                              ▼
                    DevMan Local API
                              │
                              ▼
                       DevMan Daemon
                              │
               ┌──────────────┼───────────────┐
               ▼              ▼               ▼
            Process         Docker          Health
```

任何 GUI 能做的事情最终都必须可以通过 API / CLI 完成。

---

# 3. V0.1 技术栈

## Core / Daemon

```text
Language: Go
```

职责：

- Process Supervisor
- Project Registry
- Port Manager
- Log Manager
- Health Checker
- Config Parser
- SQLite Storage
- Local HTTP API
- WebSocket / SSE
- Docker command adapter

---

## Desktop GUI

```text
Tauri 2
React
TypeScript
Vite
```

职责：

- Project Dashboard
- Service Dashboard
- Start / Stop / Restart
- Logs Viewer
- Port Viewer
- Project Registration
- Config Editor
- Environment Status
- Settings

---

## Database

```text
SQLite
```

第一版禁止引入 PostgreSQL 等外部数据库。

---

## Project Definition

```text
devman.yaml
```

项目运行拓扑采用 YAML 声明。

---

## CLI

```text
devman
```

Go 编译出的单二进制。

---

## AI Integration

第一阶段：

```text
Codex Skill
```

第二阶段预留：

```text
MCP Server
```

---

# 4. 总体架构

```text
┌──────────────────────────────────────────────┐
│                 DevMan Desktop               │
│                                              │
│ React UI                                     │
│                                              │
│ Projects / Services / Logs / Ports / Config │
└──────────────────────┬───────────────────────┘
                       │
                       │ localhost HTTP
                       ▼
┌──────────────────────────────────────────────┐
│                 DevMan Daemon                │
│                                              │
│ Project Manager                              │
│ Process Supervisor                           │
│ Port Manager                                 │
│ Log Manager                                  │
│ Health Manager                               │
│ Runtime Adapter                              │
│ Registry                                     │
│                                              │
└────────────┬────────────┬────────────┬────────┘
             │            │            │
             ▼            ▼            ▼
        Host Process    Docker      OS / TCP
             │
             ▼
        Application
```

---

# 5. Daemon 架构

建议 Go 包结构：

```text
internal/
├── api/
├── config/
├── daemon/
├── project/
├── service/
├── process/
├── runtime/
├── port/
├── health/
├── log/
├── registry/
├── storage/
├── events/
├── platform/
└── security/
```

---

# 6. Runtime 抽象

第一版设计统一 Runtime Interface：

```go
type Runtime interface {
    Start(ctx context.Context, service Service) (*ProcessHandle, error)
    Stop(ctx context.Context, handle ProcessHandle) error
    Kill(ctx context.Context, handle ProcessHandle) error
    Status(ctx context.Context, handle ProcessHandle) (ProcessStatus, error)
}
```

第一版实现：

```text
HostRuntime
DockerComposeRuntime
```

未来：

```text
DockerRuntime
WSLRuntime
SSHRuntime
KubernetesRuntime
```

但 V0.1 不实现。

---

# 7. Host Runtime

支持：

```text
Node
Python
Go
Rust
Java
PHP
Ruby
任意 executable
任意 shell command
```

例如：

```yaml
command: pnpm
args:
  - dev
```

优先使用 executable + args，而不是全部作为 shell string。

原因：

```text
Windows
macOS
Linux
```

shell 规则不同。

只有明确指定：

```yaml
shell: true
```

时才经过 shell。

---

# 8. 跨平台 Shell

支持：

```yaml
shell: true
command: pnpm dev && echo ready
```

平台实现：

Windows：

```text
cmd.exe /D /S /C
```

PowerShell 可额外：

```yaml
shell:
  type: powershell
```

执行：

```text
powershell.exe -NoProfile -Command
```

macOS/Linux：

```text
/bin/sh -c
```

未来可以支持：

```text
bash
zsh
fish
```

---

# 9. 进程管理

Process Supervisor 是整个系统最重要的模块。

每个运行实例必须产生：

```text
ProcessInstance
```

字段至少：

```text
instance_id
project_id
service_id
pid
started_at
stopped_at
exit_code
status
restart_count
command
cwd
runtime
```

状态：

```text
STOPPED
STARTING
RUNNING
STOPPING
FAILED
CRASHED
UNKNOWN
```

---

# 10. Process Tree 管理

禁止只 kill 父 PID。

例如：

```text
npm
 ↓
node
 ↓
vite
```

停止 service 时必须终止整个 process tree。

平台分别实现：

```text
platform/process_windows.go
platform/process_unix.go
```

Windows：

使用 Job Object 或等价 process-tree 管理。

macOS / Linux：

使用 Process Group。

目标：

```text
devman stop frontend
```

必须保证：

```text
pnpm
node
vite
child processes
```

全部退出。

---

# 11. Daemon 生命周期

GUI 和 Daemon 必须解耦。

关闭 GUI：

```text
默认不停止服务
```

因为用户可能只是关掉管理窗口。

Daemon 可以继续运行。

系统托盘：

```text
DevMan Running
```

用户可以：

```text
Open DevMan
Stop All Projects
Quit DevMan
```

真正 Quit DevMan 时：

弹出：

```text
Keep services running
Stop all services
Cancel
```

---

# 12. 项目配置文件

推荐：

```text
devman.yaml
```

基本结构：

```yaml
version: 1

project:
  name: my-ai-project

services:
  frontend:

    runtime: host

    cwd: ./frontend

    command: pnpm
    args:
      - dev

    port:
      env: PORT
      value: auto

    health:
      type: http
      url: http://127.0.0.1:${PORT}

  backend:

    runtime: host

    cwd: ./backend

    command: uv
    args:
      - run
      - uvicorn
      - app.main:app
      - --reload
      - --port
      - ${PORT}

    port:
      env: PORT
      value: auto

    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
```

---

# 13. 完整 Service Schema

V0.1 支持：

```yaml
services:

  backend:

    display_name: Backend API

    runtime: host

    cwd: ./backend

    command: uv

    args:
      - run
      - uvicorn
      - app.main:app

    shell: false

    env_file:
      - .env
      - .env.local

    env:
      NODE_ENV: development

    ports:
      - name: http
        value: auto
        env: PORT

    depends_on:
      - redis

    health:
      type: http
      url: http://127.0.0.1:${PORT}/health
      interval: 5s
      timeout: 3s
      retries: 10

    restart:
      policy: on-failure
      max_attempts: 3

    autostart: false
```

---

# 14. Port Manager

这是 V0.1 核心卖点之一。

必须单独设计：

```text
Port Manager
```

而不是启动前临时检测端口。

---

# 15. Port Registry

SQLite 保存：

```text
port
project_id
service_id
port_name
pid
status
allocated_at
released_at
```

状态：

```text
RESERVED
BOUND
RELEASED
CONFLICT
```

---

# 16. 自动端口

支持：

```yaml
ports:
  - name: http
    value: auto
```

DevMan：

1. 获取 preferred range。
2. 查 SQLite reservation。
3. 查 OS 是否占用。
4. 选择可用端口。
5. RESERVED。
6. 注入环境变量。
7. 启动服务。
8. 验证监听。
9. BOUND。

例如：

```text
frontend → 3000
frontend2 → 3001
backend → 8000
backend2 → 8001
```

---

# 17. Preferred Port

允许：

```yaml
ports:
  - name: http
    preferred: 3000
    value: auto
```

逻辑：

```text
3000 available
→ use 3000

3000 occupied
→ find next available
```

---

# 18. 固定端口

支持：

```yaml
ports:
  - name: http
    value: 8000
```

如果 8000 已占用：

```text
PORT_CONFLICT
```

禁止偷偷更改固定端口。

GUI 提示：

```text
Port 8000 is currently used by:

project-b/backend

或者

PID 4512 python.exe
```

---

# 19. Port Range

Global Settings：

```yaml
port_ranges:

  frontend:
    start: 3000
    end: 3999

  backend:
    start: 8000
    end: 8999

  general:
    start: 10000
    end: 19999
```

Service 可声明：

```yaml
port:
  range: frontend
```

---

# 20. Port Environment Injection

例如：

```yaml
ports:

  - name: http
    value: auto
    env: PORT
```

DevMan 自动：

```text
PORT=3014
```

然后执行：

```text
pnpm dev
```

---

# 21. Template Variables

必须实现配置变量解析。

第一版：

```text
${PORT}
${PORT:http}
${PROJECT_DIR}
${SERVICE_DIR}
${HOME}
```

以及环境变量：

```text
${ENV:OPENAI_API_KEY}
```

---

# 22. 多端口 Service

例如：

```yaml
ports:

  - name: http
    value: auto
    env: PORT

  - name: debug
    value: auto
    env: DEBUG_PORT
```

可解析：

```text
${PORT:http}
${PORT:debug}
```

---

# 23. Health Check

支持三种：

```text
process
tcp
http
```

---

# 24. Process Health

默认：

```yaml
health:
  type: process
```

PID 还活着即可。

---

# 25. TCP Health

```yaml
health:
  type: tcp
  host: 127.0.0.1
  port: ${PORT}
```

---

# 26. HTTP Health

```yaml
health:

  type: http

  url: http://127.0.0.1:${PORT}/health

  expected_status:
    - 200
    - 204
```

---

# 27. Service 状态和 Health 分离

必须区分：

```text
Process Status

RUNNING
```

和：

```text
Health

HEALTHY
UNHEALTHY
CHECKING
UNKNOWN
```

例如：

```text
backend

Process: RUNNING
Health: UNHEALTHY
```

GUI 显示：

```text
● process running
! health failing
```

---

# 28. 项目综合状态

Project Status：

```text
STOPPED
STARTING
HEALTHY
DEGRADED
FAILED
STOPPING
```

例如：

```text
frontend HEALTHY
backend HEALTHY
redis HEALTHY
worker CRASHED
```

Project：

```text
DEGRADED
```

---

# 29. 服务依赖

支持：

```yaml
depends_on:
  - postgres
  - redis
```

第一版启动：

```text
dependency graph
```

执行拓扑排序。

例如：

```text
postgres
   ↓
backend
   ↓
worker
```

---

# 30. 依赖健康条件

支持：

```yaml
depends_on:

  redis:
    condition: started

  backend:
    condition: healthy
```

第一版可以只支持：

```text
started
healthy
```

---

# 31. Restart Policy

支持：

```yaml
restart:

  policy: no
```

```yaml
restart:

  policy: on-failure
```

```yaml
restart:

  policy: always
```

再加：

```text
max_attempts
delay
```

---

# 32. 日志系统

Daemon 捕获：

```text
stdout
stderr
```

所有输出必须带：

```text
timestamp
project
service
stream
```

---

# 33. 日志存储

建议：

```text
~/.devman/logs/
```

结构：

```text
logs/
└── project-id/
    ├── frontend.log
    ├── backend.log
    └── worker.log
```

必须做 rotation。

例如：

```text
10 MB / file
5 backups
```

---

# 34. 实时日志

GUI 使用 WebSocket 或 SSE：

```text
Daemon
   ↓
stream
   ↓
GUI
```

支持：

```text
pause
resume
search
filter stdout
filter stderr
clear view
```

clear 只清界面，不删除日志文件。

---

# 35. 日志错误摘要

V0.1 可以提供：

```text
Last Error
```

例如取 stderr 最近 20 行。

API：

```text
GET /services/:id/errors/latest
```

让 AI 可以快速获得故障上下文。

---

# 36. Environment 管理

第一版只引用：

```text
.env
.env.local
.env.development
```

禁止做自己的 Secret Vault。

配置：

```yaml
env_file:
  - .env
  - .env.local
```

---

# 37. Environment Validation

允许声明：

```yaml
required_env:

  - DATABASE_URL
  - OPENAI_API_KEY
```

DevMan 启动前验证。

缺失：

```text
SERVICE_BLOCKED

OPENAI_API_KEY missing
```

---

# 38. GUI 中不能泄露 Secret

GUI 只能显示：

```text
OPENAI_API_KEY    configured
DATABASE_URL      configured
S3_SECRET         missing
```

默认不能显示值。

---

# 39. Project Registry

用户可以通过：

```text
GUI
CLI
AI Skill
```

注册项目。

SQLite：

```text
projects
```

字段：

```text
id
name
path
config_path
created_at
updated_at
last_started_at
favorite
```

---

# 40. 注册项目

CLI：

```bash
devman register .
```

或者：

```bash
devman register /projects/my-app
```

流程：

```text
Locate devman.yaml
      ↓
Validate schema
      ↓
Resolve paths
      ↓
Check services
      ↓
Check commands
      ↓
Check duplicate project
      ↓
Register
```

---

# 41. 无配置注册

如果没有：

```text
devman.yaml
```

执行：

```bash
devman register .
```

返回：

```text
No devman.yaml found.

Run:

devman init
```

---

# 42. devman init

第一阶段 CLI 可以提供基础自动探测：

```bash
devman init
```

识别：

```text
package.json
pnpm-workspace.yaml
yarn.lock
package-lock.json
pyproject.toml
requirements.txt
go.mod
Cargo.toml
docker-compose.yml
compose.yml
```

但：

> **复杂项目自动识别优先由 Codex Skill 完成。**

这样第一版 Core 不必内置复杂 AI。

---

# 43. Codex Skill

项目仓库必须包含：

```text
skills/
└── devman/
    ├── SKILL.md
    ├── references/
    │   ├── schema.md
    │   ├── detection-rules.md
    │   └── examples.md
    └── scripts/
        └── validate-config.*
```

Skill 名称建议：

```text
devman-project-manager
```

---

# 44. Skill 主要职责

Codex 使用该 Skill 后必须知道：

1. 如何判断项目是否安装 DevMan。
2. 如何判断是否存在 `devman.yaml`。
3. 如何分析项目架构。
4. 如何识别 frontend / backend / worker。
5. 如何判断正确启动命令。
6. 如何判断工作目录。
7. 如何判断端口。
8. 如何生成 `devman.yaml`。
9. 如何验证配置。
10. 如何调用 `devman register`。
11. 后续如何通过 DevMan 启停服务。
12. 不应再通过 Codex shell 长期运行这些服务。

---

# 45. Codex 检测规则

Skill 指导 Codex优先读取：

```text
package.json
pyproject.toml
docker-compose.yml
compose.yml
README.md
Makefile
Taskfile.yml
Procfile
.env.example
```

Node：

检查：

```json
"scripts": {
  "dev": "...",
  "start": "..."
}
```

Python：

检查：

```text
FastAPI
Flask
Django
Celery
RQ
uvicorn
gunicorn
```

---

# 46. Skill 自动生成流程

用户：

```text
把这个项目加入 DevMan
```

Codex 应执行：

```text
1. Scan repository
2. Detect services
3. Determine commands
4. Determine cwd
5. Determine dependencies
6. Determine ports
7. Determine health endpoints if available
8. Generate devman.yaml
9. Run devman validate
10. Fix errors
11. Run devman register .
12. Return registered services
```

---

# 47. AI 不允许盲目猜命令

如果：

```text
package.json
```

明确：

```json
"dev": "next dev"
```

应该：

```yaml
command: pnpm
args:
  - dev
```

而不是：

```yaml
command: next
```

Skill 必须规定：

> 优先使用项目已有 package scripts / documented commands，不重新发明启动命令。

---

# 48. Skill 生成后的验证

必须执行：

```bash
devman validate
```

成功：

```text
✓ config valid
✓ frontend command found
✓ backend command found
✓ no static port collision
✓ dependency graph valid
```

---

# 49. Skill 注册

然后：

```bash
devman register .
```

返回：

```text
Registered:

my-project

frontend
backend
worker
```

---

# 50. Codex 后续行为

如果项目存在：

```text
devman.yaml
```

并且 DevMan 已安装：

Codex 在启动长期开发服务时应：

```text
优先 devman
```

禁止默认：

```text
pnpm dev
```

一直挂在 Codex shell。

应该：

```bash
devman start my-project frontend
```

或者：

```bash
devman start .
```

---

# 51. CLI 设计

第一版：

```bash
devman init

devman validate

devman register
devman unregister

devman list

devman status

devman start
devman stop
devman restart

devman logs

devman ports

devman open
```

---

# 52. 当前目录模式

必须支持：

```bash
cd my-project

devman start
```

自动读取当前项目。

也支持：

```bash
devman start my-project
```

---

# 53. 单服务操作

```bash
devman start backend

devman stop backend

devman restart backend
```

或者显式：

```bash
devman service restart my-project backend
```

第一版最好支持简写。

---

# 54. devman status

示例：

```text
my-project

SERVICE      STATUS      HEALTH       PORT
frontend     RUNNING     HEALTHY      3001
backend      RUNNING     HEALTHY      8012
worker       RUNNING     -
redis        RUNNING     HEALTHY      6379
```

---

# 55. devman ports

```text
PORT     PROJECT       SERVICE
3001     my-project    frontend
8012     my-project    backend
6379     my-project    redis
```

---

# 56. Local API

Daemon 默认：

```text
127.0.0.1
```

禁止：

```text
0.0.0.0
```

第一版只允许本机访问。

---

# 57. API 版本

统一：

```text
/api/v1/
```

---

# 58. Project API

```text
GET    /api/v1/projects
POST   /api/v1/projects/register

GET    /api/v1/projects/{id}
DELETE /api/v1/projects/{id}

POST   /api/v1/projects/{id}/start
POST   /api/v1/projects/{id}/stop
POST   /api/v1/projects/{id}/restart
```

---

# 59. Service API

```text
GET /api/v1/projects/{id}/services

POST /api/v1/projects/{id}/services/{service}/start
POST /api/v1/projects/{id}/services/{service}/stop
POST /api/v1/projects/{id}/services/{service}/restart
```

---

# 60. Logs API

```text
GET /api/v1/projects/{id}/services/{service}/logs
```

Live：

```text
GET /api/v1/events
```

推荐 SSE。

---

# 61. Port API

```text
GET /api/v1/ports
```

```text
GET /api/v1/ports/{port}
```

---

# 62. Health API

```text
GET /api/v1/projects/{id}/health
```

---

# 63. Daemon API Authentication

虽然只监听：

```text
127.0.0.1
```

仍建议启动时生成：

```text
local auth token
```

保存于：

```text
~/.devman/auth-token
```

GUI / CLI 自动读取。

避免任意网页直接调用本地 daemon。

---

# 64. GUI 信息架构

主导航：

```text
Projects
Ports
Logs
Settings
```

---

# 65. Projects 首页

示意：

```text
DevMan

Projects                       + Register

● CRM
  4 / 4 running
  HEALTHY

  frontend      :3000    ●
  backend       :8000    ●
  redis         :6379    ●
  worker                 ●

○ Image Tool
  stopped

● Test App
  2 / 3 running
  DEGRADED
```

---

# 66. Project Detail

```text
CRM

Start All
Stop All
Restart All

Services

frontend
RUNNING
HEALTHY
http://localhost:3000

backend
RUNNING
HEALTHY
http://localhost:8000

worker
CRASHED
exit code 1
```

---

# 67. Service Detail

显示：

```text
Status
Health
PID
Uptime
Command
CWD
Ports
Restart Count
Last Exit Code
```

操作：

```text
Start
Stop
Restart
Open URL
Logs
```

---

# 68. Logs GUI

要求：

```text
real-time
auto-scroll
pause
search
stdout/stderr filter
service selector
```

不要第一版做复杂日志分析平台。

---

# 69. Ports GUI

页面：

```text
Ports

3000    CRM/frontend
3010    Image/frontend
6379    CRM/redis
8000    CRM/backend
8010    Image/backend
```

点击显示：

```text
PID
service
project
process
started
```

---

# 70. Register Project GUI

支持拖入文件夹：

```text
Drop project folder here
```

或者：

```text
Choose Folder
```

检测：

```text
devman.yaml
```

存在：

```text
Validate → Register
```

不存在：

提示：

```text
No DevMan configuration found.

Create manually
Open documentation
Use Codex to configure
```

---

# 71. Config 页面

V0.1 可以提供：

```text
raw YAML editor
```

旁边实时：

```text
Valid
Invalid
```

不需要第一版实现复杂可视化配置 builder。

---

# 72. Docker Compose Runtime

支持：

```yaml
runtime: docker-compose

compose:
  file: docker-compose.yml
  service: redis
```

启动：

```text
docker compose up -d redis
```

停止：

```text
docker compose stop redis
```

状态：

```text
docker compose ps
```

日志：

```text
docker compose logs
```

---

# 73. Docker 检测

如果 Docker 不存在：

状态：

```text
BLOCKED
```

原因：

```text
Docker executable not found
```

不是：

```text
FAILED
```

---

# 74. External Service

建议第一版增加：

```yaml
runtime: external
```

例如本机已有 PostgreSQL：

```yaml
postgres:

  runtime: external

  health:
    type: tcp
    host: 127.0.0.1
    port: 5432
```

DevMan：

```text
只监控
不启动
不停止
```

非常实用。

---

# 75. Startup Modes

Project：

```yaml
startup:
  default:
    - frontend
    - backend
```

这样：

```bash
devman start
```

只启动 default。

完整启动：

```bash
devman start --all
```

---

# 76. Profiles

第一版可预留但不完整实现：

```yaml
profiles:

  default:
    - frontend
    - backend

  full:
    - frontend
    - backend
    - worker
    - scheduler
```

如果开发成本低可以直接实现。

CLI：

```bash
devman start --profile full
```

---

# 77. GUI 后台启动

Desktop App 启动时：

```text
Check daemon
```

未运行：

```text
start daemon
```

Daemon 已运行：

```text
connect
```

---

# 78. 系统开机启动

Settings：

```text
Start DevMan on login
```

默认：

```text
OFF
```

可以允许：

```text
Daemon auto start
GUI auto start
```

分开控制。

---

# 79. GUI PATH 问题

这是三端必须重点处理的问题。

GUI 应用从系统菜单启动时，尤其 macOS/Linux，通常不能简单假设它拥有用户交互式 shell 的完整 PATH。

因此 DevMan 必须有：

```text
Environment Resolver
```

启动时探测：

```text
node
npm
pnpm
yarn
bun
python
python3
uv
poetry
go
cargo
docker
```

GUI Settings 显示：

```text
pnpm      /opt/homebrew/bin/pnpm
node      /opt/homebrew/bin/node
python    /usr/local/bin/python3
docker    /usr/local/bin/docker
```

并允许：

```text
Additional PATH
```

---

# 80. Command Resolution

配置：

```yaml
command: pnpm
```

注册时解析实际路径：

```text
/opt/homebrew/bin/pnpm
```

但不要永久把平台绝对路径写回：

```text
devman.yaml
```

因为配置必须可以 Git 跨平台共享。

Resolved executable 属于：

```text
runtime state
```

不是项目配置。

---

# 81. 平台特定配置

允许：

```yaml
platform:

  windows:
    env: {}

  macos:
    env: {}

  linux:
    env: {}
```

以及特殊 command override：

```yaml
command:

  default: pnpm

  windows: pnpm.cmd
```

但第一版 AI 应尽量生成跨平台配置。

---

# 82. Path 规则

项目 YAML 一律：

```text
相对项目根目录
```

推荐：

```yaml
cwd: ./backend
```

禁止 AI 默认生成：

```text
C:\Users\...
```

或：

```text
/Users/xxx/...
```

---

# 83. SQLite Schema

核心表：

```text
projects
services_runtime
process_instances
port_allocations
events
settings
```

---

# 84. projects

```text
id TEXT PK
name TEXT
path TEXT UNIQUE
config_path TEXT
created_at
updated_at
last_started_at
favorite
```

---

# 85. process_instances

```text
id
project_id
service_name
pid
status
runtime
started_at
stopped_at
exit_code
restart_count
```

---

# 86. port_allocations

```text
id
port
project_id
service_name
port_name
state
allocated_at
released_at
```

---

# 87. Event Bus

Daemon 内部所有状态变化发事件：

```text
PROJECT_REGISTERED
PROJECT_STARTED

SERVICE_STARTING
SERVICE_STARTED
SERVICE_STOPPED
SERVICE_CRASHED

PORT_RESERVED
PORT_BOUND
PORT_RELEASED

HEALTH_CHANGED
```

GUI 订阅事件。

未来 MCP 也能利用。

---

# 88. Crash Recovery

Daemon 自己崩溃或机器重启后：

SQLite 中可能还认为：

```text
PID 1234 RUNNING
```

Daemon 启动必须进行：

```text
reconciliation
```

检查：

```text
PID 是否存在
进程 identity 是否匹配
端口是否绑定
```

然后修正状态。

---

# 89. PID 复用问题

禁止仅凭 PID 判断。

存储：

```text
pid
started_at
command fingerprint
```

尽量检测是否还是原始进程。

---

# 90. 服务停止超时

默认：

```text
graceful_timeout: 10s
```

流程：

```text
SIGTERM / graceful signal
     ↓
wait
     ↓
force kill process tree
```

Windows 对应平台机制。

---

# 91. 安全策略

Project Config 本质允许运行命令：

```yaml
command:
```

因此注册未知仓库时 GUI 必须展示：

```text
This project can execute local commands.
```

第一次注册：

展示将运行：

```text
frontend

cwd:
./frontend

command:
pnpm dev
```

用户确认后注册。

AI 自动注册也不能绕过安全模型。

---

# 92. Config Schema

项目提供：

```text
devman.schema.json
```

支持 VS Code YAML autocomplete。

在：

```yaml
# yaml-language-server
```

中可引用。

---

# 93. validate 命令

```bash
devman validate
```

必须验证：

```text
schema
duplicate service names
cwd existence
command syntax
dependency cycles
port definitions
health definitions
template variables
env_file path
docker config
```

---

# 94. validate --json

为了 AI 集成：

```bash
devman validate --json
```

返回：

```json
{
  "valid": false,
  "errors": [
    {
      "path": "services.backend.cwd",
      "message": "directory does not exist"
    }
  ]
}
```

非常重要。

---

# 95. CLI JSON Mode

所有 AI 重要命令应支持：

```text
--json
```

例如：

```bash
devman status --json
devman ports --json
devman register . --json
devman logs backend --json
```

不要让 Codex解析 GUI 或彩色 Terminal 文本。

---

# 96. AI-friendly Error Model

统一错误码：

```text
CONFIG_INVALID
PROJECT_NOT_FOUND
SERVICE_NOT_FOUND
PORT_CONFLICT
COMMAND_NOT_FOUND
ENV_MISSING
DEPENDENCY_FAILED
HEALTHCHECK_FAILED
PROCESS_CRASHED
DOCKER_NOT_FOUND
```

API 和 CLI 共用。

---

# 97. 第一版 MCP 规划

V0.1 主交付可以先不依赖 MCP。

但 Core API 必须保证未来很容易加入：

```text
devman-mcp
```

推荐工具：

```text
devman_list_projects
devman_get_project_status

devman_start_project
devman_stop_project
devman_restart_project

devman_start_service
devman_stop_service
devman_restart_service

devman_get_logs
devman_get_ports
devman_get_health
```

MCP Server 只是：

```text
MCP → localhost DevMan API
```

不能重新实现 process manager。

---

# 98. 推荐 Monorepo

```text
devman/

├── apps/

│   └── desktop/
│       ├── src/
│       └── src-tauri/

├── cmd/

│   └── devman/

├── internal/

│   ├── api/
│   ├── daemon/
│   ├── process/
│   ├── project/
│   ├── service/
│   ├── port/
│   ├── health/
│   ├── log/
│   ├── runtime/
│   ├── storage/
│   └── platform/

├── pkg/

│   └── config/

├── skills/

│   └── devman-project-manager/

│       ├── SKILL.md
│       ├── references/
│       └── scripts/

├── schemas/

│   └── devman.schema.json

├── examples/

│   ├── next-fastapi/
│   ├── react-express/
│   ├── next-python-worker/
│   └── docker-mixed/

├── docs/

│   ├── config.md
│   ├── cli.md
│   └── architecture.md

├── go.mod
├── package.json
└── README.md
```

---

# 99. 推荐开发阶段

## Phase 1 — Core Process Engine

先禁止开发复杂 GUI。

完成：

```text
config parser
process start
process stop
process tree
stdout/stderr
service status
```

验收：

```bash
devman start
devman stop
devman status
```

Windows / macOS / Linux 全通过。

---

## Phase 2 — Project Registry

完成：

```text
SQLite
register
unregister
list
```

验收：

```bash
devman register .
devman list
```

---

## Phase 3 — Port Manager

完成：

```text
automatic port
fixed port
reservation
collision detection
environment injection
```

验收：

同时启动两个：

```text
port 3000 preferred
```

自动：

```text
3000
3001
```

---

## Phase 4 — Health + Dependencies

完成：

```text
HTTP
TCP
process
depends_on
restart
```

---

## Phase 5 — Log System

完成：

```text
stdout
stderr
rotation
streaming
```

---

## Phase 6 — HTTP API

把 CLI 已有能力全部暴露。

原则：

```text
CLI → Core
API → Core
```

不是：

```text
CLI → API
```

或者可以采用：

```text
CLI → Daemon API
```

但必须只存在一套 business logic。

推荐最终：

```text
Core Service Layer
      ↑
 ┌────┴────┐
 API      embedded CLI bootstrap
```

---

## Phase 7 — Desktop GUI

先做：

```text
Projects
Project Detail
Logs
Ports
Settings
```

---

## Phase 8 — Codex Skill

实现：

```text
project detection
config generation
validation
registration
runtime usage rules
```

---

## Phase 9 — Packaging

Windows：

```text
MSI / NSIS
```

macOS：

```text
DMG
```

Linux：

优先：

```text
AppImage
```

其次：

```text
deb
rpm
```

---

# 100. V0.1 必须测试的项目类型

必须建立真实 fixtures。

## Fixture A

```text
React/Vite
+
Express
```

---

## Fixture B

```text
Next.js
+
FastAPI
```

---

## Fixture C

```text
React
+
FastAPI
+
Celery
+
Redis Docker
```

---

## Fixture D

```text
Frontend Host
Backend Host
Postgres Docker
Redis Docker
AI Worker Python
```

这个是最重要的真实测试。

---

# 101. 跨平台 CI

GitHub Actions：

```text
windows-latest
macos-latest
ubuntu-latest
```

运行：

```text
Go tests
config tests
port tests
process tests
frontend tests
build
```

---

# 102. Process Integration Test

测试：

启动 fixture：

```text
test-server
```

等待：

```text
health
```

检查：

```text
PID
port
logs
```

停止。

确认：

```text
parent gone
child gone
port released
```

---

# 103. Port Race Test

必须专门测试：

```text
10 services
```

同时请求：

```text
auto port
```

确保不会获得同一端口。

Port reservation 必须具有数据库 / 内存锁。

---

# 104. V0.1 GUI 设计原则

界面不是 Docker Desktop。

要求：

```text
简单
快
不打扰
开发者导向
```

颜色主要用于：

```text
Running
Healthy
Warning
Error
Stopped
```

项目卡片必须优先展示：

```text
当前是否运行
几个服务
哪些端口
是否健康
```

---

# 105. V0.1 明确不做

第一版严禁需求膨胀。

不做：

```text
Kubernetes
Cloud deployment
SSH runtime
Remote server
Team workspace
RBAC
Secret vault
CI/CD
Git hosting
Built-in AI model
Container engine
Database management
Reverse proxy
HTTPS certificate
Remote logs
Observability platform
Metrics dashboard
```

---

# 106. 第一版成功标准

V0.1 成功不取决于功能数量。

应该满足以下场景：

```text
我 clone 一个 AI 项目。

告诉 Codex：

“把这个项目加入 DevMan。”
```

Codex：

```text
分析 repository
     ↓
生成 devman.yaml
     ↓
validate
     ↓
register
```

打开 DevMan：

```text
My AI Project

○ frontend
○ backend
○ worker
○ redis
```

点击：

```text
Start All
```

变成：

```text
● frontend :3012 HEALTHY
● backend  :8014 HEALTHY
● worker         RUNNING
● redis    :6379 HEALTHY
```

之后 Codex 修改 backend。

Codex 不运行：

```text
uvicorn ...
```

而运行：

```bash
devman restart backend
```

用户无需打开额外 Terminal。

这就是 V0.1 的核心成功标准。

---

# 107. 第一版最关键的技术优先级

如果实现 AI 必须在技术决策之间排序，优先顺序固定为：

```text
1. Process lifecycle reliability
2. Cross-platform behavior
3. Port management correctness
4. Configuration stability
5. Logs
6. Health checking
7. CLI/API reliability
8. Codex integration
9. GUI polish
```

漂亮 GUI 永远不能优先于：

```text
服务能不能可靠启动和关闭
```

---

# 108. 第一版产品底线

DevMan 必须满足：

> 启动一个服务以后，即使关闭 Codex、关闭 Terminal、关闭 DevMan GUI，这个服务仍然由 DevMan Daemon 正确管理。

以及：

> 使用 DevMan 停止服务以后，不应留下它创建的孤儿子进程和被错误占用的端口。

以及：

> 同一个项目配置必须尽可能能在 Windows、macOS、Linux 三个平台共享。

以及：

> Codex 不需要理解 DevMan 内部实现，只需要通过 Skill 知道如何生成、验证、注册和操作项目。

这四条属于 V0.1 不可妥协要求。

---

# 109. 建议开发顺序

代码 AI 收到本方案后，不要一次性生成整个项目。

严格按以下顺序提交：

```text
M1
Repository scaffold
Config types
Schema
Tests

M2
Process supervisor
Cross-platform process tree
Tests

M3
Log capture
Process status

M4
Project registry
SQLite

M5
Port manager
Auto allocation
Conflict detection

M6
Health checks
Dependencies
Restart policy

M7
Daemon API
Events

M8
CLI

M9
Desktop shell
Project GUI

M10
Logs GUI
Ports GUI
Settings

M11
Codex Skill

M12
Packaging
Cross-platform CI
Documentation
```

每一个 Milestone 都必须处于：

```text
buildable
testable
usable
```

状态后，才能进入下一阶段。

---

# 110. V0.1 最终交付物

最终仓库至少需要：

```text
devman daemon/core

devman CLI

Windows desktop application

macOS desktop application

Linux desktop application

devman.yaml schema

JSON schema

Codex Skill

example projects

documentation

cross-platform CI

release packaging
```

最终用户体验：

```text
Install DevMan
       ↓
Install DevMan Codex Skill
       ↓
Open project
       ↓
Ask Codex:
“Configure this project for DevMan.”
       ↓
devman.yaml generated
       ↓
project registered
       ↓
Open GUI
       ↓
Start / Stop / Restart / Logs / Ports
```

到这里，DevMan V0.1 就构成一个完整、独立、可日常使用的产品，而不是一个单纯的进程启动器。