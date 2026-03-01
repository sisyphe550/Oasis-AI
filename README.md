# Oasis-AI
A modular, offline-first AI orchestrator for multi-model pipelines and private context management.

Oasis-AI webUI为拥有ollama环境的用户提供了简便易用的操纵界面

1.打开命令行运行ollama，确认ollama已经安装（若对安装有疑问请看https://github.com/alix0830/Linux-install-ollama进行快速部署）

2.创建Modelfile，把它放在本地gguf文件的同一目录下

3.Modelfile文件中写入：FROM (同目录下gguf文件的路径)

4.同目录下运行ollama create my-model -f Modelfile (my-model可以替换为任何你喜欢的名称，需在.html中相应修改)

5.返回主目录输入ollama list测试模型是否成功添加

6.输入sudo systemctl stop ollama.service 停止ollama服务

7.（再次检查）输入lsof -iTCP:11434 -sTCP:LISTEN 若显示PID，输入kill -9 (PID)

8.OLLAMA_ORIGINS=* ollama serve （特别注意，此命令可能带来安全问题，建议使用纯离线环境运行或指定端口）

9.编辑.html文件，找到 const MODEL = 'my-model'; 确认''内是你要运行的模型名称

10.（可选）打开保存.html的目录，打开终端输入python -m http.server 8090

11.使用浏览器打开.html文件，或直接在浏览器打开本地路径


目前已完成：

轻量级，可离线部署✅️

可视化界面✅️

用户自定义提示词✅️

向量数据库✅️

AI输出调整✅️

多提示词协作✅️

存在漏洞：

设置并行路径时路径崩溃

预计修复：

重构路径部分，让用户可以配置各节点的顺序执行或并行执行，如：A1-B1//B2-A2，A1接收用户出入，B1,B2接收A1的输入内容，A2接收B1,B2的输出内容，最后输出给用户，用户可自由配置。另外需要让用户随时编辑节点路径。用户需要可以配置节点的权限等级：普通等级：只能读取上一个节点的输出，不能读取用户的输入内容（上一个节点是用户除外），高级等级：可以读取上一个节点的输出和用户的输出，系统等级：此等级节点可以读取上一个节点的输出和用户的输出，并且有在此链路增加或删除节点的权限。

未完成：

多AI模型协作

可爱的操作界面
