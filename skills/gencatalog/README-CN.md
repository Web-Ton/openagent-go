# gencatalog — 刷新 `INSTALLABLE_SKILLS.json`

`skills/gencatalog/main.py` 是一个 Python 脚本，用于将
`skills/INSTALLABLE_SKILLS.json` 与远端 skill 仓库
`https://gitcode.com/huaweicloud/huaweicloud-skills.git` 保持同步。

在以下场景运行此工具：

1. **远端某个 skill 的内容变了** —— 重算它的 `skill_folder_md5` 并刷新该条目。
2. **远端新增了 skill** —— 报告为候选，确认后再写入。

## 为什么 `skill_folder_md5` 必须是*远端*目录的 MD5

catalog 在运行时由 `skill-manager.wasm` 插件消费
（见 `feat/skill-upgrade` 分支的 `examples/plugin/plugins/skill-manager/src/lib.rs`）。
其 `is_upgradeable` 逻辑是：

```rust
let local = host::directory_md5(&format!("{}/{}", skills_dir, name)); // 本地已安装目录
local != remote_md5   // remote_md5 == catalog 里的 skill_folder_md5
```

`host::directory_md5` 调用 `plugin/wasmhost/fs.go` 的 `DirectoryMD5`，后者调用
`skill/fs.FolderMD5(path, filepath.Base(path))` —— **与本工具使用的算法相同**
（下方用 Python 重新实现）。因此 catalog 的 `skill_folder_md5` 会和一个用**相同算法**
算出的**本地已安装目录** MD5 比较。要让这个比较能探测到远端变化，catalog 里的值必须是
**远端**目录的 MD5。如果存的是本地 MD5，两者永远相等，升级就永远探测不到。

因为 `FolderMD5` 不依赖父路径（只依赖目录名 + 文件内容），对远端的本地浅克隆算出的
MD5 与对真实远端目录算出的 MD5 **完全一致**。已验证：三个已知 skill 的 catalog MD5 与
从本地克隆重算的值精确匹配。

## MD5 算法

定义在 [`skill/fs/md5.go`](../fs/md5.go) 的 `FolderMD5(dirpath, dirname)`，由 `main.py`
里的 `folder_md5()` 逐字节镜像：

```
entries = [dirname]
深度优先遍历；每一层：
  文件（按名排序）  -> 追加 "relpath:md5(文件字节)"
  子目录（按名排序）-> 递归
result = hex(md5("\n".join(entries)))
```

- **目录名**参与计算（重命名目录会改变 MD5）；**父路径不参与**（移动目录不会）。
- **文件内容**参与计算。
- **跳过符号链接、FIFO、设备、socket**（与 Go 的 `walkSorted` 对齐；也避免对无写入端的
  FIFO 永久阻塞）。
- 对**整个 skill 目录**计算（SKILL.md + references/ + scripts/ + 所有文件），不是只算
  SKILL.md。

`folder_md5()` 已验证与 Go `FolderMD5` 在已知 skill 上输出一致 —— Python 和 Go 实现可互换。

## 用法

```sh
# 原地刷新 catalog：克隆远端、重算 MD5、覆盖 MD5 变化的条目。
# 新增/删除的 skill 只报告，不自动写入。
python3 skills/gencatalog/main.py

# 预演（dry run）：打印摘要，不写文件。
python3 skills/gencatalog/main.py --dry-run

# 确认候选后，显式新增指定的 skill。
python3 skills/gencatalog/main.py --add=huawei-cloud-dew-key-management,huawei-cloud-cts-trace-management

# 复用已有克隆（省去下载，适合离线迭代）。
python3 skills/gencatalog/main.py --clone-dir=/tmp/huaweicloud-skills-probe

# 校验：对 catalog 每个 skill 重算远端 folder_md5，与 catalog 里的
# skill_folder_md5 做完整 32 字符比对（不是只比前几位）。
# 不写文件。任何刷新/新增后都应跑一次，替代人工肉眼比对。
python3 skills/gencatalog/main.py --verify

# 补全：把 skill_md 与远端 SKILL.md 不一致的条目（历史简化版、内容过时
# 或有笔误的）刷新为远端完整原文。不动 skill_folder_md5（远端目录没变）。
python3 skills/gencatalog/main.py --complete
```

需要 Python 3.7+ 和 `pyyaml`（`pip install pyyaml`），无其它依赖。

选项：

| 选项          | 默认值                                                          | 含义                                                             |
| ------------- | --------------------------------------------------------------- | ---------------------------------------------------------------- |
| `--out`       | `skills/INSTALLABLE_SKILLS.json`                                | 读写 catalog 路径                                                |
| `--clone-dir` | （临时目录）                                                    | 复用此目录作为克隆根（若其中已有 `skills/` 则跳过克隆）          |
| `--dry-run`   | 关                                                              | 打印计划，不写文件                                               |
| `--add`       | （无）                                                          | 逗号分隔的远端 skill 名，指定要新增到 catalog 的 skill           |
| `--verify`    | 关                                                              | 校验每个条目的 `skill_folder_md5` 与远端重算值完整一致；不写文件，不一致时退出码 1 |
| `--complete`  | 关                                                              | 把 `skill_md` 与远端不一致的条目刷新为远端完整原文；不动 `skill_folder_md5` |

## 做什么 —— 以及不做什么

**做：**

- 浅克隆远端仓库到临时目录（退出时清理）。
- 对每个远端 skill，用 `folder_md5`（插件的算法）重算 `skill_folder_md5`，并读取完整
  `SKILL.md`。
- 对每个**已有**的 catalog 条目：若 MD5 变了，用新 MD5、**完整** `skill_md`、解析后的
  `frontmatter`、重新生成的 `install_cmd` / `remove_cmd` 覆盖该条目。若未变，原样保留。
- skills 数组按 name 排序（发布文件就是按 name 排序的）。
- 原子写入（`.tmp` + `os.replace`）。

**不做：**

- **不自动新增**远端独有的 skill。它们以 `new candidates` 形式打印，并给出确切的
  `--add=...` 命令。需显式新增。
- **不自动删除**catalog 独有的 skill。它们以 `REMOVED candidates` 形式打印。确认后手动
  删除。
- 不动 MD5 未变的条目 —— 包括约 20 个历史上**简化**的条目（其 `skill_md` 被砍成只剩
  frontmatter 以缩小文件）。这些条目保持简化状态，直到其远端内容真正变化时才刷新为完整
  `SKILL.md`。**新增 skill 时，务必使用完整 `SKILL.md`** —— 不要模仿简化条目；它们是一次性
  缩体积的产物，不是可参考的格式。

## 为什么用 Python（不用 Go）

catalog 文件本身就是 `json.dumps(d, ensure_ascii=False, indent=2)` 的产物（已验证：经
Python `json` 往返与发布文件字节一致）。Python 的 `dict` 保留插入顺序（3.7+），
`json.dumps(ensure_ascii=False)` 不会对 `<`/`>`/`&` 做 HTML 转义，也没有尾换行的问题。
因此重新序列化一个未变化的条目是字节一致的，**零自定义机械代码**。

Go 版则需要为**每一层嵌套**写自定义 ordered-map 类型 —— `frontmatter` 的值本身是对象和
对象数组（`trigger{keywords,resource_types,hypotheses}`、
`input_schema{required,optional}`、`output_schema[{name,type,description}]`），而 Go 的
`map` 不保序、`encoding/json` 会按字母序排 map key 且默认 HTML 转义。要递归地把这些全做对
很容易出 bug（之前的 Go 版就重排了嵌套 key、转义了 HTML、多加了尾换行）。Python 免费避开
了所有这些坑。

## catalog 条目格式

2 空格缩进，固定字段顺序：

```json
{
  "name": "huawei-cloud-<...>",
  "frontmatter": { "name": "...", "description": "...", "...": "..." },
  "skill_md": "---\nname: ...\n---\n\n# 完整 SKILL.md 正文...\n",
  "install_cmd": { "cmd": "npx", "args": ["-y", "skills", "add", "<url>", "--skill", "<name>", "-g", "-y"] },
  "remove_cmd": { "cmd": "npx", "args": ["-y", "skills", "remove", "<name>", "-g", "-y"] },
  "skill_folder_md5": "<32 位十六进制>"
}
```

- `frontmatter` 是 `SKILL.md` 的**完整** YAML frontmatter —— 包含所有 key
  （`name`、`description`、`tags`、`version`、`allowed-tools`、`compatibility`、…），
  顺序保持 SKILL.md 作者书写的原始顺序，不只是 `name`/`description`。
- `skill_md` 是**完整**的 `SKILL.md` 文本（frontmatter + 正文），CRLF 归一化为 LF。
- `install_cmd` / `remove_cmd` 全部使用同一个远端 URL。

## 远端仓库目录结构

```
huaweicloud-skills/
└── skills/
    ├── agentorchard/common/huawei-cloud-agentorchard-find-skills/SKILL.md
    ├── ai/modelarts/huawei-cloud-ascend-command/SKILL.md
    ├── bigdata/dws/huawei-cloud-dws-io-diag/SKILL.md
    └── ...   (skills/<category>/<subcat?>/<skill-name>/SKILL.md)
```

一个 skill 就是任何包含 `SKILL.md` 的目录。skill 的**名**是该目录的基本名（叶子），
与 catalog 的 `name` 字段一致。

## 验证清单

真实运行后：

1. `python3 -m json.tool skills/INSTALLABLE_SKILLS.json > /dev/null` —— JSON 合法。
2. **`python3 skills/gencatalog/main.py --verify`** —— 对每个 skill 重算远端
   `folder_md5`，与 catalog 里的 `skill_folder_md5` 做**完整 32 字符比对**。这是机器校验，
   不依赖模型或人肉眼比对，能抓出"只比前几位""字符数不一致"等错误。不一致时退出码 1。
3. `git diff skills/INSTALLABLE_SKILLS.json` —— 只有预期条目变化；缩进和字段顺序与前文件一致。
4. 抽查一个**未变化**且有嵌套 frontmatter 的条目（如 `huawei-cloud-dws-cpu-diag`）：
   其 `trigger` key 仍是 `keywords, resource_types, hypotheses`（源顺序，非字母序）——
   证明未变化条目字节不动。
5. 抽查一个**更新**或**新增**的条目：其 `skill_folder_md5` 等于远端重算值
   （`--verify` 已自动做这个比对）。
6. 条目数：不带 `--add` 运行 `python3 skills/gencatalog/main.py` 后数量不变；只有加了
   `--add=...` 才会增加。

## wasm 插件兼容性

catalog 由 `skill-manager.wasm` 插件（`feat/skill-upgrade` 分支
`examples/plugin/plugins/skill-manager/src/lib.rs`）消费。插件用 `serde_json::Value`
解析 catalog，按 `.get("字段").as_str()` 访问 —— **不依赖 key 顺序**，只要求字段存在、
类型正确。本脚本产出的 catalog 已验证：103 个条目，插件期望的 `name`/`frontmatter`/
`skill_md`/`install_cmd`/`remove_cmd`/`skill_folder_md5` 字段全部存在且类型正确，0 问题。

关键的 `skill_folder_md5` 与插件一致：插件 `is_upgradeable` →
`host::directory_md5` → `plugin/wasmhost/fs.go` `DirectoryMD5` →
`skill/fs.FolderMD5(path, filepath.Base(path))`，与本脚本的 `folder_md5()` 是同一算法
（已验证输出一致）。插件算**本地已安装目录**，catalog 存**远端目录**的值，二者用相同算法
才能比较出"可升级"。
