<p align="center">
  <img src="docs/assets/brand/mark.png" alt="" width="128">
</p>

<h1 align="center">Anchored</h1>

<p align="center">
  Uma só memória para todos os agentes de código da sua máquina.
</p>

<p align="center">
  <a href="https://github.com/jholhewres/anchored/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/jholhewres/anchored/ci.yml?branch=main&style=flat-square&label=ci" alt="CI"></a>
  <a href="https://github.com/jholhewres/anchored/releases/latest"><img src="https://img.shields.io/github/v/release/jholhewres/anchored?style=flat-square&color=e2795b" alt="Última versão"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/licen%C3%A7a-MIT-blue?style=flat-square" alt="Licença: MIT"></a>
  <img src="https://img.shields.io/badge/go-1.25+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.25+">
</p>

<p align="center">
  <a href="README.md">English</a> &middot; <b>Português</b>
</p>

---

## O que é

Uma memória local que seus agentes de código compartilham. Claude Code,
Cursor, Codex, OpenCode, Gemini CLI e qualquer outra ferramenta MCP leem e
gravam no mesmo banco na sua máquina. Uma decisão tomada numa sessão continua
lá na próxima, seja qual for a ferramenta que você abrir. Um binário, sem
conta, e nada sai da máquina a menos que você adicione um remote.

## O que faz

- **Um banco para todas as ferramentas**: o servidor MCP e a CLI são um binário só, e o `anchored init` o registra em mais de quinze hosts.
- **Busca que acha o que você quis dizer**: BM25, embeddings multilíngues locais, entidades e um grafo de conhecimento, ranqueados para o projeto em que você está; cerca de 0,2 s com 85 mil memórias.
- **Contexto no início de cada sessão**: quem você é, o projeto e as decisões recentes, dentro de um orçamento de tokens, com a economia contabilizada (`anchored stats --tokens`).
- **Um grafo de conhecimento** do que usa, possui e depende do quê, por projeto.
- **Memória que se mantém limpa**: a curadoria em segundo plano rebaixa entradas de pouco valor sem apagar nada; o `dream` propõe fusões e aponta contradições para você revisar.
- **Ferramentas para a janela de contexto**: rodar código, indexar saídas longas e documentação baixada, e buscar nelas sem inundar o modelo.
- **Remotes para times**: compartilhe a memória dos projetos por uma instância [Anchored OSS](https://github.com/jholhewres/anchored_oss) hospedada por você, pessoal e da empresa lado a lado, com cada repositório indo para a sua.
- **Privado por padrão**: só o conhecimento de projeto que passa no filtro de segurança é sincronizado; segredos, caminhos locais e o seu escopo pessoal ficam em casa.
- **Um dashboard local** com as memórias, o grafo, as tarefas, a economia de tokens e as ferramentas conectadas.

## Instalação

```sh
curl -fsSL https://raw.githubusercontent.com/jholhewres/anchored/main/install/install.sh | bash
```

Linux, macOS e Windows pelo Git Bash. A primeira execução baixa o modelo de
embeddings local (~470 MB); o `anchored self-update` mantém o binário em dia.
Pelo código-fonte, atualizações e notas por plataforma:
[docs/install.md](docs/install.md) (em inglês).

## Conecte suas ferramentas

Claude Code, com o plugin (MCP, slash commands, hooks e skill de uma vez):

```text
/plugin marketplace add jholhewres/anchored
/plugin install anchored@anchored
```

Todo o resto:

```sh
anchored init                  # detecta as ferramentas instaladas
anchored init --tool cursor    # ou uma de cada vez
```

Cada host, arquivo e hook: [docs/setup.md](docs/setup.md) (em inglês).

## Links

- [O que faz, em detalhe](docs/features.md)
- [CLI e ferramentas MCP](docs/cli.md)
- [Remotes e sync de time](docs/remotes.md) · [Anchored OSS](https://github.com/jholhewres/anchored_oss)
- [Design](docs/design.md) · [Arquitetura](docs/architecture.md) · [Modelo de embeddings](docs/embedding-model.md)
- [Fontes de importação](docs/import-sources.md) · [Protocolo MCP](docs/mcp-protocol.md) · [Glossário](GLOSSARY.md)
- [Changelog](CHANGELOG.md) · [Como contribuir](CONTRIBUTING.md)
- [Licença: MIT](LICENSE)

A documentação em `docs/` está em inglês.
