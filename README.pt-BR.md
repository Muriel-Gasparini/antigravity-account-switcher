# Antigravity Account Switcher

[English](README.md) | [Português (Brasil)](README.pt-BR.md)

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.24%2B-00ADD8?logo=go)](https://go.dev/)
[![CI Status](https://github.com/Muriel-Gasparini/antigravity-account-switcher/actions/workflows/ci.yml/badge.svg)](https://github.com/Muriel-Gasparini/antigravity-account-switcher/actions)
[![CodeRabbit Pull Request Reviews](https://img.shields.io/coderabbit/prs/github/Muriel-Gasparini/antigravity-account-switcher?utm_source=oss&utm_medium=github&utm_campaign=Muriel-Gasparini%2Fantigravity-account-switcher&labelColor=171717&color=FF570A&link=https%3A%2F%2Fcoderabbit.ai&label=CodeRabbit+Reviews)](https://coderabbit.ai)

Gerenciamento automático de pool de contas, monitoramento de cotas em tempo real e failover transparente em erros HTTP 429 para o **Google Antigravity 2.0** e CLI (`agy`).

---

## O que é o Antigravity 2.0 e por que esta ferramenta é necessária?

O **Google Antigravity 2.0** é o novo aplicativo desktop de desenvolvimento com IA criado pela equipe do Google DeepMind (completamente independente e diferente das antigas extensões de preview para VS Code).

Ao trabalhar intensamente com o Antigravity 2.0, é muito comum atingir os limites de requisições (`HTTP 429 RESOURCE_EXHAUSTED` ou a janela deslizante de 5 horas de cota) nos modelos Claude, GPT e Gemini.

O **Antigravity Account Switcher** roda como um supervisor local transparente e de alta performance que intercepta as requisições do Antigravity 2.0. Quando a conta ativa esgota sua cota, o switcher **alterna instantaneamente para a próxima conta disponível no seu pool e repete a requisição em memória** — sem interromper o raciocínio do agente, sem quebrar os streams de resposta e sem gerar erros na interface.

---

## Instalando o Google Antigravity 2.0 no Linux

> [!IMPORTANT]
> **O Google Antigravity 2.0 NÃO é distribuído via gerenciadores de pacotes (`apt`, `snap`, `dnf`, `pacman` ou `flatpak`).**  
> O Google disponibiliza a aplicação diretamente como um arquivo `.tar.gz` contendo o binário executável e as bibliotecas necessárias.

No Linux, programas distribuídos dessa forma devem ser instalados ou na **Pasta de Usuário XDG** (recomendado, não exige root nem `sudo`) ou na **Pasta do Sistema `/opt`** (requer privilégios de administrador).

### Opção 1: Instalação de Usuário (Recomendada — Sem necessidade de `sudo`)

Essa opção segue a especificação **Linux XDG Base Directory** (`~/.local/share/` e `~/.local/bin/`). Ela não afeta outros usuários da máquina nem arquivos do sistema operacional:

```bash
# 1. Cria a pasta da aplicação
mkdir -p ~/.local/share/antigravity

# 2. Descompacta o arquivo baixado do Google
tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C ~/.local/share/antigravity --strip-components=1

# 3. Garante permissão de execução no binário
chmod +x ~/.local/share/antigravity/antigravity

# 4. Cria o atalho em ~/.local/bin (já presente no PATH das principais distribuições Linux)
mkdir -p ~/.local/bin
ln -sf ~/.local/share/antigravity/antigravity ~/.local/bin/antigravity
```

### Opção 2: Instalação no Sistema (Requer `sudo` / Padrão FHS)

Segue o padrão **Filesystem Hierarchy Standard (FHS)** para softwares de terceiros em `/opt`:

```bash
# 1. Cria a pasta no sistema
sudo mkdir -p /opt/antigravity

# 2. Descompacta o arquivo
sudo tar -xzf ~/Downloads/Antigravity-linux-x64.tar.gz -C /opt/antigravity --strip-components=1

# 3. Cria o link simbólico global
sudo ln -sf /opt/antigravity/antigravity /usr/local/bin/antigravity
```

---

## Como o Switcher se Conecta ao Antigravity 2.0

### Detecção Automática (Zero Configuração)
Se você seguiu qualquer um dos métodos de instalação recomendados acima, **você não precisa configurar nenhum caminho manualmente**.  
Ao executar `antigravity-account-switcher launch`, o switcher busca automaticamente o executável nos caminhos padrão do Linux:
1. `~/.local/bin/antigravity`
2. `~/.local/share/antigravity/antigravity`
3. `/usr/local/bin/antigravity`
4. `/opt/antigravity/antigravity`
5. `~/tools/Antigravity/Antigravity-x64/antigravity` *(fallback para pastas personalizadas)*
6. Qualquer comando `antigravity` ou `agy` presente no `$PATH` do seu sistema

### Configuração de Caminho Personalizado
Caso tenha descompactado o Antigravity 2.0 em uma pasta diferente, você pode especificar o caminho facilmente por qualquer um destes métodos:

**Método 1: Configuração permanente via CLI (Recomendado)**
```bash
antigravity-account-switcher config set antigravity_bin /caminho/para/seu/antigravity
```

**Método 2: Variável de Ambiente**
```bash
export ANTIGRAVITY_BIN="/caminho/para/seu/antigravity"
```

**Método 3: Flag no momento da execução**
```bash
antigravity-account-switcher launch --bin /caminho/para/seu/antigravity
```

---

## Início Rápido (3 Passos)

### 1. Compilar e Instalar o Switcher
```bash
git clone https://github.com/Muriel-Gasparini/antigravity-account-switcher.git
cd antigravity-account-switcher
make install
```
*Compila um único binário estático (sem dependências CGO) e o instala em `~/.local/bin/antigravity-account-switcher`.*

### 2. Cadastrar suas Contas
Autentique uma ou mais contas do Google com fluxo OAuth seguro:
```bash
antigravity-account-switcher add-account
```
*(Se você já possui o Antigravity 2.0 instalado e com login ativo, o switcher importa automaticamente sua conta no primeiro uso!)*

### 3. Iniciar o Antigravity 2.0
```bash
antigravity-account-switcher launch
```
O switcher inicia o proxy em segundo plano, abre o Antigravity 2.0 com variáveis de ambiente isoladas e monitora a saúde das contas. Ao fechar o Antigravity 2.0, o switcher é encerrado automaticamente no mesmo instante.

---

## Integração com o Desktop (GNOME / KDE / XFCE)

Para abrir o Antigravity 2.0 diretamente do menu de aplicativos ou da barra de tarefas/dock do seu Linux:

```bash
antigravity-account-switcher install-desktop
```
Esse comando automaticamente:
- Localiza o binário do Antigravity 2.0 na sua máquina.
- Extrai e configura o ícone oficial da aplicação em `~/.local/share/icons/antigravity.png`.
- Cria o arquivo de atalho `~/.local/share/applications/antigravity.desktop` apontando para `antigravity-account-switcher launch %F`.

Para remover o atalho desktop a qualquer momento:
```bash
antigravity-account-switcher uninstall-desktop
```

---

## Referência de Comandos da CLI

A CLI disponibiliza comandos para supervisão, troca manual e configuração:

| Comando | Descrição |
| :--- | :--- |
| `launch` | **(Recomendado)** Inicia o Antigravity 2.0 sob a supervisão do proxy acoplado. |
| `serve` | Inicia o proxy local, monitor de cotas em segundo plano e dashboard web como serviço. |
| `wrap -- <comando>` | Executa qualquer comando arbitrário injetando o proxy apenas naquele processo. |
| `add-account` | Inicia o fluxo OAuth2 loopback (RFC 8252) no navegador para cadastrar uma nova conta Google. |
| `list-accounts` | Exibe todas as contas cadastradas, qual está ativa e as porcentagens de cota. |
| `refresh-quotas` | Força a sincronização imediata de cotas com o Google para todas as contas. |
| `status` | Mostra a conta ativa no momento, métricas de tokens e saúde do switcher. |
| `config` | Consulta ou atualiza configurações persistentes (`get`, `set`, `list`). |
| `install-desktop` | Cria o atalho `.desktop` no menu do GNOME/XDG com o ícone oficial. |
| `uninstall-desktop`| Remove o atalho `.desktop` do menu do sistema. |
| `version` | Exibe a versão compilada, hash do commit e data de build. |

### Flags Úteis

- **Abrir o Dashboard Web no Navegador ao Iniciar:**
  ```bash
  antigravity-account-switcher launch --open
  ```
- **Adicionar Conta em Servidor Remoto / Sem Navegador (SSH / Headless):**
  ```bash
  antigravity-account-switcher add-account --no-browser
  ```
- **Especificar Porta Customizada:**
  ```bash
  antigravity-account-switcher launch --port 1831
  ```

---

## Referência de Configuração

As configurações ficam salvas em formato JSON em `~/.config/antigravity-account-switcher/config.json`.

```bash
# Ver todas as configurações atuais
antigravity-account-switcher config list

# Definir caminho do executável do Antigravity 2.0
antigravity-account-switcher config set antigravity_bin ~/.local/share/antigravity/antigravity

# Definir porta padrão do dashboard web
antigravity-account-switcher config set port 1831

# Ajustar intervalo de checagem de cotas em segundo plano
antigravity-account-switcher config set quota_interval 60s
```

### Variáveis de Ambiente

| Variável | Descrição |
| :--- | :--- |
| `ANTIGRAVITY_BIN` | Caminho explícito para o executável do Antigravity 2.0. |
| `ANTIGRAVITY_PORT` | Sobrescreve a porta do proxy e dashboard. |
| `ANTIGRAVITY_DB_PATH` | Caminho para o banco de dados SQLite (padrão: `~/.config/.../accounts.db`). |
| `ANTIGRAVITY_CLIENT_ID` | Sobrescrita opcional do Client ID do Google Cloud Console. |
| `ANTIGRAVITY_CLIENT_SECRET` | Sobrescrita opcional do Client Secret do Google Cloud Console. |

---

## Arquitetura

```text
               +-----------------------------------+
               |          Antigravity 2.0          |
               |    (Processo Filho via Supervisor)|
               +-----------------+-----------------+
                                 | HTTP_PROXY / CLOUD_CODE_URL
                                 v
+------------------------------------------------------------------+
|              ANTIGRAVITY ACCOUNT SWITCHER (Binário Único)        |
|                                                                  |
|   +--------------------+     +-------------------------------+   |
|   |   Dashboard Web    |     |     Proxy Reverso em Processo |   |
|   |  (HTML5/Tailwind)  |     | * Bearer Token Dinâmico       |   |
|   |  http://127.0.0.1  |     | * Buffer de Replay de 100MB   |   |
|   +---------+----------+     | * Túnel RFC 7231 CONNECT      |   |
|             |                +---------------+---------------+   |
|             v                                |                   |
|   +--------------------+         HTTP 429    | Tokens SSE        |
|   | Banco SQLite WAL   |<--------------------+                   |
|   | * accounts.db      |                     v                   |
|   | * métricas tokens  |     +-------------------------------+   |
|   +---------+----------+     | Daemon de Monitor de Cotas    |   |
|             ^                | * Auto-restaura após o reset  |   |
|             +----------------+ * User-Agent oficial Google PA|   |
+----------------------------------------------+-------------------+
                                               |
                                               v
                              +--------------------------------+
                              | Infraestrutura Google CloudCode|
                              +--------------------------------+
```

---

## Solução de Problemas & Perguntas Frequentes (FAQ)

#### 1. "Could not automatically locate Antigravity binary"
Se a sua instalação do Antigravity 2.0 estiver em um diretório personalizado que não foi detectado automaticamente, aponte para o executável:
```bash
antigravity-account-switcher config set antigravity_bin /caminho/para/antigravity
```

#### 2. Isso interfere na digitação por voz (Speech-to-Text) do Antigravity?
Não. O switcher define automaticamente `NO_PROXY=speech.googleapis.com` e implementa tunelamento TCP bruto via RFC 7231 em conexões `CONNECT`, garantindo latência zero e funcionamento perfeito do áudio.

#### 3. Onde ficam guardados meus tokens e credenciais?
Os tokens ficam armazenados exclusivamente no seu disco local, no banco SQLite protegido em `~/.config/antigravity-account-switcher/accounts.db`. Nenhuma informação, token ou métrica jamais sai da sua máquina.

#### 4. Como faço para desinstalar completamente e apagar todos os dados?
```bash
# 1. Remove o atalho do menu desktop
antigravity-account-switcher uninstall-desktop

# 2. Remove o binário
make uninstall

# 3. Exclui a pasta de configurações e o banco de dados
rm -rf ~/.config/antigravity-account-switcher
```

#### 5. A autoatualização nativa ("Check for Updates") funciona?
Sim! No Linux, o Antigravity 2.0 utiliza o mecanismo `AppImageUpdater` do Electron. Ao rodar o Antigravity a partir do arquivo `.tar.gz` descompactado sem um runtime de AppImage, a opção `Help -> Check for Updates` costuma falhar com `ERR_UPDATER_OLD_FILE_NOT_FOUND` pela ausência da variável de ambiente `APPIMAGE`.

**O Antigravity Account Switcher corrige isso de fábrica.** Ao iniciar via `antigravity-account-switcher launch` (ou pelo atalho desktop criado por `install-desktop`), o supervisor injeta automaticamente a variável `APPIMAGE` apontando para o binário correto no processo do Antigravity 2.0, permitindo que a verificação de atualizações e a atualização automática funcionem perfeitamente.

---

## Segurança

Consulte [SECURITY.md](SECURITY.md) para detalhes sobre políticas de reporte de vulnerabilidades e conformidade com a especificação RFC 8252 §8.5 para clientes OAuth 2.0 públicos.

---

## Como Contribuir

Contribuições são muito bem-vindas! Consulte o arquivo [CONTRIBUTING.md](CONTRIBUTING.md) para instruções de ambiente de desenvolvimento, execução dos testes com detector de race e padrões de Pull Request.

---

## Licença

Distribuído sob a licença MIT © 2026 Muriel Gasparini. Veja [LICENSE](LICENSE) para mais detalhes.
