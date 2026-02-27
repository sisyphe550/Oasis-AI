初始化并连接到远程 Git 仓库是一个非常基础且核心的开发步骤。以下是为完整操作流程，你可以直接在终端（Terminal）中按顺序执行这些命令。

### 第一步：进入项目目录并初始化本地仓库

首先，确保你的终端已经进入了你想要作为项目根目录的文件夹中。如果还没有创建文件夹，可以先创建并进入：

```bash
mkdir Oasis-AI
cd Oasis-AI

```

然后，将这个目录初始化为一个 Git 仓库：

```bash
git init

```

*执行后，Git 会在当前目录下创建一个隐藏的 `.git` 文件夹，这标志着本地仓库创建成功。*

---

### 第二步：与远程仓库建立连接

使用 `git remote add` 命令将你的本地仓库与 GitHub 上的远程仓库关联起来。通常我们会将这个远程仓库命名为 `origin`（这是业界的习惯命名）：

```bash
git remote add origin https://github.com/sisyphe550/Oasis-AI.git

```

**验证连接是否成功：**
你可以运行以下命令来查看当前配置的远程仓库地址：

```bash
git remote -v

```

*如果配置正确，你应该会看到两行输出，分别对应 `fetch` 和 `push` 的远程地址。*

---

### 第三步：同步代码（根据你的实际情况选择）

建立连接后，你需要进行第一次代码同步。这里通常有两种情况：

**情况 A：远程仓库里已经有代码（例如别人创建的，或者你在网页端初始化了 README）**
你需要先将远程的代码拉取到本地：

```bash
git pull origin main

```

*(注意：如果远程仓库的默认分支是 `master`，请将 `main` 替换为 `master`。)*

**情况 B：远程是空仓库，你准备把本地已经写好的代码推送到远程**
你需要将本地文件添加到暂存区、提交，并推送到远程仓库：

```bash
# 1. 将所有文件添加到暂存区
git add .

# 2. 提交代码并添加注释
git commit -m "Initial commit"

# 3. 确保你的主分支名称为 main (GitHub 默认标准)
git branch -M main

# 4. 将本地的 main 分支推送到 origin，并建立追踪关系 (-u)
git push -u origin main

```

---
