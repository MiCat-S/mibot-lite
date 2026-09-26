# MiBot Lite

[简体中文](README.md) | **English** | [繁體中文](README.zh-TW.md) | [日本語](README.ja.md)

A memory-light Telegram userbot: one static Go binary, with its commands compiled in and its state kept in JSON files.

It is a lightweight stand-in for [MiBox](https://github.com/MiCat-S/Mi-Box). The account, the session, `config.json`
and the command names are all the same, so you can switch over from MiBox directly and bring its settings along with a
single command. What it leaves out is the plugin system: no JavaScript runtime, no SQLite, and less than half the
resident memory of the Node version of MiBox.

| | MiBox (Node) | MiBot Lite |
|---|---|---|
| Resident memory (RSS) | about 125 MB | **about 30–50 MB** |
| Install size | node_modules, about 237 packages | **one file, about 21 MB** |
| Plugin runtime | V8 | none |
| Storage | better-sqlite3 | JSON files |
| Image processing | sharp (libvips) | standard library + `golang.org/x/image` |
| Adding a command | `.tpm install` | change the code and rebuild |

Memory was measured on the same production machine: about 30 MB right after connecting and about 50 MB after a few
hours, nearly 20 MB of which are pages mapped in from the binary itself. The ffmpeg child process that runs briefly
while making stickers is not included (see [Design notes](#design-notes)).

## Install

On the server (Linux amd64 / arm64, systemd, root):

```sh
bash <(curl -fsSL https://raw.githubusercontent.com/MiCat-S/mibot-lite/main/scripts/install.sh)
```

It downloads, verifies, logs you in and installs the service in one go. Running it again upgrades; after that you can
also send `.update run` in Telegram. The step-by-step procedure, settings and memory limits are in
[INSTALL.md](INSTALL.md) (Chinese).

- **ffmpeg**: needed by `.eatgif`, `.eat`, `.eat2` and `.t`, and by `.yvlu` when it quotes a video. Without it the
  other commands work as usual. `apt install -y ffmpeg`.
- **Moving from MiBox**: settings for ai, sum, whois, aliases, prefixes and more can be carried over directly:

  ```sh
  ./mibot-lite --import-mibox /root/mibot --root /root/mibot-lite
  ```

  The same account can switch back and forth between the two programs, but **they must not run at the same time**:
  Telegram makes two connections of one account knock each other off.
- **Reinstalling or moving to a new machine**: send `.bf` in Telegram to back up the configuration to Saved Messages.
  On the new machine, pass `--restore <backup file>` to the installer to restore everything as it was, without logging
  in again. See [INSTALL.md section 10](INSTALL.md#10-备份与恢复重装系统换机器).

## Commands

The default prefixes are `.`, `。` and `$`; change them with `.prefix`. For a command's full usage send
`.help <command>`, or add `--help` after the command.

The "Lend" column says whether the command can be lent to other people through
[`.sudo` / `.sure`](#lending-the-account-sudo-and-sure): ✓ means the whole command can be lent; **partly** means
only the parts for looking things up and using it can be lent, while subcommands that change settings stay with the
owner; blank means owner only.

**Running and maintenance**

| Command | What it does | Lend |
|---|---|---|
| `.ping [domain]` | Latency to Telegram or a website | ✓ |
| `.status` | Status card (CPU, memory, disk, swap) | ✓ |
| `.memory` | Process memory | ✓ |
| `.sysinfo` | Detailed system information | |
| `.version` `.ver` | Version information | ✓ |
| `.help` `.h` | Command list, or the help for one command | ✓ |
| `.update [check\|run\|rollback]` | Check for, install or roll back an update | |
| `.restart` | Restart the service | |
| `.log` | Export the run log (sensitive values removed) | |
| `.bf` | Back up the configuration to Saved Messages | |
| `.prefix` `.alias` | Change the command prefixes, give commands aliases | |
| `.privacy` | How IP addresses in the output are masked | |

**Lookups and tools**

| Command | What it does | Lend |
|---|---|---|
| `.calc <expression>` | Arithmetic | ✓ |
| `.rate <currency> [target] [amount]` | Exchange rates and conversion | ✓ |
| `.tr [language] <text>` | Google Translate, no setup needed | ✓ |
| `.gt [language] <text>` | AI translation | ✓ |
| `.whois <domain>` | Domain registration data, in batches too | partly |
| `.ip [IP\|domain]` | Location and network of an IP | ✓ |
| `.bin <first 6–8 card digits>` | Issuing bank of a card BIN | ✓ |
| `.ids` `.dc` | Profile of a user or chat, and its data center | ✓ |
| `.speedtest` `.st` | Server speed test (official Ookla CLI) | partly |

**AI**

| Command | What it does | Lend |
|---|---|---|
| `.ai [search] <question>` | AI chat and web search | partly |
| `.sum [count]` | Group chat summaries, also on a schedule | partly |

**Messages and stickers**

| Command | What it does | Lend |
|---|---|---|
| `.yvlu` | Turn messages into a quote sticker, image or story | partly |
| `.eatgif <name>` | Animated sticker made from both people's avatars | partly |
| `.eat` `.eat2` | Meme sticker made from an avatar or a picture | partly |
| `.sticker` | Save a sticker into your own sticker pack | |
| `.t <text>` | Text to speech (`.ts` picks a voice, `.tk` sets the API key) | |
| `.re [messages] [times]` | Repeat the replied-to message | ✓ |
| `.save <link>` | Save or forward messages, even where forwarding is restricted | |
| `.dme <count>` | Delete your own messages | |
| `.da` | Delete group messages in bulk | |

**Group administration**

| Command | What it does | Lend |
|---|---|---|
| `.ban` `.unban` `.kick` `.mute` `.unmute` | Ban, kick or mute in the current group | ✓ |
| `.sb` `.unsb` | Ban or unban in every group you administer | |
| `.refresh` | Refresh the list of administered groups (it refreshes on its own once a day) | |
| `.aban` | Help for the commands above | |

**Account**

| Command | What it does | Lend |
|---|---|---|
| `.acn` `.autochangename` | Change your display name automatically by time and weather | |
| `.sudo` `.sure` | Lend commands to people on a list, see below | |

### Lending the account: .sudo and .sure

Both let people on a list run commands through your account: they post a message in a group, and the account sends
the command as you, in the same chat, replying to the same target. With `.sudo` they send commands directly; `.sure`
is narrower: their message has to match a rule you set, and it can be rewritten, for example turning a member's
`/sb` into `.ban`.

Unlike MiBox, **what can be lent is an allowlist**, the "Lend" column in the tables above. MiBox lets people on the
list run any command, `.sudo add` included, so anyone you authorize can authorize others. Here the check is made on
the real command after aliases are expanded, so an alias cannot get around it, and commands or subcommands added
later cannot be lent by default. When someone on the list sends a command that cannot be lent, the account only
replies that they lack permission.

The lists are kept in `data/sudo.json` and `data/sure.json`, and `.bf` backs them up too.

### IP masking: .privacy

As in MiBox v2, IP addresses are masked in every message the account sends or edits: by default the last 2 parts of
an IPv4 address and the last 4 groups of an IPv6 address. Links pointing at an IP are removed, and IPs in file names
are masked too. `.privacy ip mask 2 4` changes how many parts are masked; `.privacy ip hide` replaces the whole
address with 「[IP已隐藏]」 ("IP hidden").

Masking happens in the connection layer, so it covers the output of every command. Commands sent on someone else's
behalf (`.sudo`, `.sure`) are not masked: those addresses were typed by that person in the first place.

## Design notes

### Why no plugin system

The MiBox host loads arbitrary TypeScript plugins, so it has to keep a JS engine, SQLite and a whole plugin lifecycle
resident, and that is where most of its memory goes. MiBot Lite drops that premise: commands are no longer artifacts
you can install at runtime but Go functions compiled into the binary. The cost is a rebuild to add a command; in
return it uses far less resident memory, and the code needs no sandbox.

### Images and video

`.yvlu` and `.eatgif` composite images. MiBox uses sharp, which binds libvips: a shared library, an image cache and a
thread pool, all resident in the process for a feature used a few times a day. Here they are replaced by the standard
library's `image/draw` plus the scalers in `golang.org/x/image`. The binary grows by only 0.7 MB, and nothing stays in
memory while idle.

The only external dependency is ffmpeg: Telegram video stickers must be VP9, and encoding that in pure Go is not
practical. ffmpeg runs as a child process and uses no memory while idle, but **it needs over 100 MB while it runs**.
That is why the systemd unit sets `MemoryMax` to 512M instead of just above the resident size, and **does not set**
`MemoryHigh`. Going over MemoryHigh does not fail; it forces reclaim again and again and slows the process to a
crawl. In one encode it fired more than 14,000 times, the encode hit its own timeout before finishing, and nothing
was left to say why.

Two differences from MiBox:

- **eatgif no longer encodes a GIF first.** The original encoded the frames as a GIF and then converted that to VP9;
  the middle step cuts every frame down to 256 colors. Here the PNG frames go straight to ffmpeg, skipping one lossy
  hop.
- **yvlu does not convert tgs animated stickers.** That path needs Python's `rlottie-python`, which was never
  installed on the production machine, so porting it would mean porting a feature that does not work today. The rest
  was matched against the original item by item: multiple messages come from the real history rather than
  consecutive IDs, forwards are credited to the original author, partial quotes, admin titles, emoji status and the
  large-avatar fallback.

## Development

```sh
go test ./...          # all tests
go vet ./...
bash scripts/build.sh  # build with a version number
```

Self-check against a real account (sends a set of read-only commands to Saved Messages, checks the replies, then
deletes them). One account cannot be connected twice at once, so stop the service first:

```sh
./mibot-lite --verify --root /deployment/dir
```

Check image compositing against real assets (skipped by default):

```sh
MIBOT_EATGIF_ASSETS=/path/to/eatgif go test ./internal/imaging/ -run RealAnimation -v
```

Code layout:

| Path | Contents |
|---|---|
| `cmd/mibot-lite` | Entry point |
| `internal/app` | Connection and update dispatch |
| `internal/bot` | Telegram wrapper, including connection-layer middleware such as flood-wait retries and IP masking |
| `internal/command` | Command registration and dispatch |
| `internal/commands/<command>/` | Command implementations: one directory per command, or per group of commands sharing data |
| `internal/commands/kit` | Small helpers shared by commands |

Commands grouped together: `aban` holds all the group administration commands, `ids` includes `.dc`, `sudo` includes
`.sure`, `eatgif` includes `.eat`, `.t` lives in `tts`, and basic commands such as `.status` are in `core`.

To add a command: create an `internal/commands/xxx/` directory with a `Register(a *app.App)` that calls
`a.Registry.Register(&command.Command{...})`, then hook it into `RegisterAll` in `internal/commands/register.go`. For it
to be lendable, it also has to be added to the allowlist in `internal/commands/sudo/sudo.go`.

The tests in the repository only cover pure logic that needs no outside environment. Behavior tests against a fake
Telegram, snapshot tests and tests that reach real services (files named `*_local_test.go`, along with
`internal/commands/testkit` and each command's `testdata`) stay on the maintainer's machine and are listed in
`.gitignore`.

## License

LGPL-2.1, the same as MiBox.

The status card font is a subset of Noto Sans SC (`internal/statuscard/NotoSansSC-status-subset.ttf`), distributed
under the SIL Open Font License 1.1; the license text is `NotoSansSC-OFL.txt` in the same directory.
