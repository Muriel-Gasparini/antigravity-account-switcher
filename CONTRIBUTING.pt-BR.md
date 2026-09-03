# Como Contribuir para o Antigravity Account Switcher

[English](CONTRIBUTING.md) | [Português (Brasil)](CONTRIBUTING.pt-BR.md)

Muito obrigado pelo interesse em contribuir com o **Antigravity Account Switcher**!

Relatórios de bugs, sugestões de melhorias, atualizações de documentação e novas funcionalidades são muito bem-vindos. Siga as orientações abaixo para garantir uma contribuição ágil e organizada.

---

## Pré-requisitos de Desenvolvimento

- **Go:** Versão 1.24+ ([Download Go](https://go.dev/dl/))
- **Git:** Cliente git padrão
- **Make:** GNU Make (opcional, mas recomendado)
- **GCC / Toolchain C:** Necessário apenas para rodar testes com o detector de race do Go (`-race`). O binário final de release compila com `CGO_ENABLED=0` (SQLite puro em Go via `modernc.org/sqlite`).
- **golangci-lint:** Versão 1.60+ ([Guia de Instalação](https://golangci-lint.run/welcome/install/))

---

## Primeiros Passos

1. **Faça o Fork e Clone o Repositório:**
   ```bash
   git clone https://github.com/Muriel-Gasparini/antigravity-account-switcher.git
   cd antigravity-account-switcher
   ```

2. **Valide seu Ambiente:**
   Execute a suíte de testes para garantir que tudo está funcionando:
   ```bash
   make test
   ```

3. **Compile o Binário Estático:**
   ```bash
   make build
   ./bin/antigravity-account-switcher version
   ```

---

## Executando Testes e Linters

Cada Pull Request é verificado automaticamente pelo GitHub Actions. Antes de enviar seu código, execute localmente:

```bash
# 1. Rodar testes unitários e de integração com detector de condições de corrida (race detector)
make test-race

# 2. Rodar análise estática e linter
make lint

# 3. Formatar o código Go no padrão oficial
make fmt
```

Todos os testes devem passar com `0` data races e o `golangci-lint` deve rodar sem nenhum aviso.

---

## Estrutura do Projeto

O projeto segue princípios de Clean Architecture / Arquitetura Hexagonal:

```text
├── cmd/
│   └── antigravity-account-switcher/ # Comandos da CLI e ponto de entrada da aplicação
├── internal/
│   ├── config/                       # Configurações persistentes (config.json) e detecção de caminhos
│   ├── domain/                       # Entidades de negócio, portas e interfaces centrais
│   ├── launcher/                     # Supervisor de processo, PR_SET_PDEATHSIG e instalador desktop
│   ├── metrics/                      # Cálculo e agregação de consumo de tokens
│   ├── oauth/                        # Servidor loopback OAuth2 (RFC 8252) e gerenciamento de tokens
│   ├── proxy/                        # Proxy reverso, motor de failover em 429 e interceptador SSE
│   ├── quota/                        # Daemon de monitoramento de cotas e detecção do language_server
│   ├── store/sqlite/                 # Banco SQLite thread-safe em modo WAL (Go puro)
│   └── web/                          # Dashboard web embutido (HTML5, Tailwind CSS, Vanilla JS)
├── scripts/                          # Scripts de automação e integração desktop
└── test/                             # Mocks e suítes de testes de integração ponta a ponta (E2E)
```

---

## Padrão de Commits (Conventional Commits)

Seguimos a especificação [Conventional Commits](https://www.conventionalcommits.org/pt-br/):

- `feat:` Nova funcionalidade
- `fix:` Correção de bug
- `docs:` Alterações na documentação
- `refactor:` Refatoração de código sem alterar comportamento
- `test:` Adição ou correção de testes
- `chore:` Scripts de build, dependências ou tarefas de manutenção

Exemplo:
```bash
git commit -m "fix(proxy): handle RFC 7231 compliance in CONNECT tunnels"
```

---

## Enviando um Pull Request

1. Crie uma branch descritiva para sua funcionalidade ou correção:
   ```bash
   git checkout -b fix/proxy-audio-bypass
   ```
2. Escreva suas alterações e adicione testes automatizados equivalentes.
3. Garanta que todos os testes e linters passam com sucesso:
   ```bash
   make lint && make test-race
   ```
4. Faça o push para o seu fork e abra o Pull Request contra a branch `main`.
5. Descreva de forma clara o problema resolvido, a solução implementada e como você validou a alteração.
