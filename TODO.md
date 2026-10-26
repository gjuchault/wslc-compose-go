# TODO — écart avec Docker Compose

Comparaison de `wslc-compose` (portage Go) avec Docker Compose v2 et la [Compose Specification](https://github.com/compose-spec/compose-spec/blob/main/spec.md).

Légende :
- **P1** : utilisé par la plupart des projets compose réels. Une absence casse ou surprend.
- **P2** : courant.
- **P3** : niche.
- 🔒 : l'original Python le déclare non supporté par `wslc`. Il faut vérifier sur la preview actuelle avant de l'implémenter.

## 1. Chargement des fichiers

| Fonctionnalité | État | Prio |
|---|---|---|
| Chargement auto de `compose.override.yaml` / `docker-compose.override.yml` | ✅ | — |
| Plusieurs `-f` fusionnés (règles de merge de la spec) et `COMPOSE_FILE` | ✅ `!reset` et `!override` compris ; `COMPOSE_FILE` lu aussi dans le `.env` | — |
| `--project-directory` | ❌ | P2 |
| Plusieurs `--env-file` | ❌ un seul | P2 |
| `env_file` en syntaxe longue (`path`, `required: false`, `format`) | ✅ `path` et `required` ; `format` ignoré | — |
| Interpolation imbriquée `${A:-${B}}` | ❌ l'opérande s'arrête au premier `}` | P2 |
| Interpolation et références `${X}` dans les valeurs du `.env` | ❌ | P2 |
| `COMPOSE_PROFILES` (variable d'environnement) | ❌ seul `--profile` est lu | P2 |
| `extends` (même fichier ou fichier externe) | ❌ | P2 |
| `include` (top-level) | ❌ | P3 |
| Ancres YAML et merge keys `<<:` | ✅ ordre de PyYAML, alias récursifs rejetés | — |
| Extensions `x-*` ignorées | ✅ à couvrir par un test | — |
| Validation du schéma (clés inconnues, types) | ❌ les clés inconnues sont ignorées en silence | P2 |

## 2. Commandes

| Commande | État | Prio |
|---|---|---|
| `run` (conteneur ponctuel, `--rm`, `--no-deps`, `-e`, `--entrypoint`, `-p/--service-ports`) | ❌ | P1 |
| `rm` (conteneurs arrêtés) | ❌ | P2 |
| `kill` (`-s SIGNAL`) | ❌ | P2 |
| `create` | ❌ | P3 |
| `images` | ❌ | P3 |
| `ls` (projets via le label `com.wslc-compose.project`) | ❌ | P2 |
| `port SERVICE PRIVATE_PORT` | ❌ | P3 |
| `top`, `stats`, `events` | ❌ | P3 |
| `pause` / `unpause` | ❌ 🔒 à vérifier | P3 |
| `cp` | ❌ | P3 |
| `push` | ❌ | P3 |
| `wait`, `watch`, `attach`, `scale`, `export`, `commit` | ❌ | P3 |

## 3. Options des commandes existantes

| Commande | Manquant | Prio |
|---|---|---|
| `up` | `--remove-orphans` (les conteneurs de services supprimés du fichier restent) | P1 |
| `up` | `--no-deps`, `--no-recreate`, `--no-start`, `--no-build`, `--pull always\|missing\|never` | P2 |
| `up` | `--abort-on-container-exit`, `--exit-code-from`, `--wait`, `--wait-timeout` | P2 |
| `up` | `--attach` / `--no-attach`, `--timestamps`, `--quiet-pull`, `-V` | P3 |
| `down` | `--remove-orphans`, `--rmi local\|all`, services en argument | P2 |
| `ps` | `-a` (les conteneurs arrêtés sont déjà listés : à aligner), `--services`, `--format json`, `--status`, `--filter` | P2 |
| `logs` | `--since`, `--until`, `--no-log-prefix`, `--no-color`, `--index`, couleurs par service | P2 |
| `exec` | `-d/--detach`, `--privileged` 🔒 | P3 |
| `build` | `--build-arg` CLI, `--pull`, `-q`, `--push` | P3 |
| `pull` | `--ignore-pull-failures`, `--include-deps`, `-q` | P3 |
| `config` | `--services`, `--volumes`, `--profiles`, `--format json`, `-q` (validation seule), sortie au format compose plutôt que modèle interne | P2 |
| global | `--parallel`, `--ansi`, `--progress`, `--compatibility`, `--all-resources` | P3 |

## 4. Clés de service

| Clé | État | Prio |
|---|---|---|
| Plusieurs `networks` par service | ✅ `create`, `network connect` (avec alias) pour chaque réseau suivant, puis `start` | — |
| `healthcheck` + `depends_on.condition: service_healthy` | ✅ flags `--health-*` (wslc 3.0.3) et polling de `inspect` ; la forme exec `CMD` devient une commande shell ; `start_interval` ignoré | — |
| `depends_on.condition: service_completed_successfully` | ✅ attente de l'exit code via `inspect` | — |
| `depends_on.required: false`, `restart: true` | ❌ | P3 |
| `restart` / `deploy.restart_policy` | ❌ 🔒 vérifié sur wslc 3.0.3 : ni `run` ni `create` n'ont de `--restart`. Avertissement conservé, bloqué côté wslc | P1 |
| `scale` (clé de service) | ❌ seul `deploy.replicas` est lu | P2 |
| `expose` | ❌ (ignoré sans avertissement) | P3 |
| `network_mode` (`host`, `none`, `service:x`) | ❌ ignoré sans avertissement | P2 |
| `ipv4_address` / `ipv6_address` / `mac_address` | ❌ | P3 |
| `links`, `external_links` | ❌ (les alias réseau couvrent le cas courant) | P3 |
| `extra_hosts` | ❌ 🔒 | P2 |
| `secrets`, `configs` | ❌ 🔒 émulables par des bind mounts read-only de fichiers | P2 |
| `stop_grace_period` | ❌ pourrait alimenter `stop -t` par service | P2 |
| `init`, `read_only`, `privileged`, `cap_add/cap_drop`, `security_opt`, `devices`, `sysctls`, `pid`, `ipc`, `userns_mode`, `group_add` | ❌ 🔒 | P3 |
| `platform`, `pull_policy` | ❌ | P2 |
| `logging` | ❌ 🔒 | P3 |
| `mem_reservation`, `memswap_limit`, `cpu_shares`, `cpuset`, `pids_limit`, `oom_*`, `deploy.resources.reservations` (hors GPU) | ❌ | P3 |
| `label_file`, `annotations`, `attach`, `post_start` / `pre_stop`, `develop.watch` | ❌ | P3 |
| `ports` syntaxe longue : `mode`, `name`, `app_protocol`, `published` sous forme de plage | ⚠️ partiel | P3 |
| `volumes` syntaxe longue : `bind.create_host_path`, `volume.nocopy`, `volume.subpath`, `tmpfs.size`, `consistency` | ❌ | P3 |
| Volumes anonymes (`- /data`) | ❌ rejetés (limite wslc selon l'original) | P2 |
| `build` : `dockerfile_inline`, `cache_from`, `labels`, `network`, `ssh`, `secrets`, `platforms`, `additional_contexts`, `shm_size` | ❌ | P3 |
| `env_file` relatifs résolus depuis le fichier qui les déclare (avec `include` / `extends`) | ❌ | P3 |

## 5. Réseaux et volumes (top-level)

| Fonctionnalité | État | Prio |
|---|---|---|
| `networks.<x>.driver`, `driver_opts`, `ipam`, `internal`, `attachable`, `labels`, `enable_ipv6` | ❌ ignorés sans avertissement | P2 |
| `volumes.<x>.driver`, `driver_opts`, `labels` | ❌ ignorés sans avertissement | P2 |
| Labels de projet sur les réseaux et volumes (pour `down` et `ls` fiables) | ❌ basé sur le nom uniquement | P2 |
| `external: {name: x}` (ancienne syntaxe) | ⚠️ `external` est vrai mais le nom imbriqué est ignoré | P3 |
| `down` qui supprime aussi le réseau `default` créé implicitement | ✅ à vérifier par un test | — |

## 6. Comportement d'orchestration

| Fonctionnalité | État | Prio |
|---|---|---|
| Démarrage parallèle des services indépendants (respect du DAG `depends_on`) | ❌ séquentiel | P2 |
| Arrêt en ordre inverse des dépendances dans `down` / `stop` | ❌ ordre de listing wslc | P2 |
| Recréation des dépendants quand une dépendance est recréée | ❌ | P3 |
| Détection et suppression des conteneurs orphelins | ❌ | P1 |
| Recréation quand l'image locale change (digest) et pas seulement la config | ❌ | P2 |
| Prise en compte du contenu des `env_file` dans le hash de config | ❌ seul le chemin est hashé | P2 |
| Logs colorés par service, préfixe `service-1 \|` | ⚠️ préfixe = nom complet du conteneur, sans couleur | P3 |
| `up` en avant-plan : Ctrl+C **arrête** les conteneurs (comportement Compose) | ⚠️ ici les conteneurs continuent de tourner (choix de l'original) | P2 |

## 7. Qualité et outillage

- [x] Compatibilité wslc 3.0.3 : JSON d'une ligne par objet pour `list` et `images`, clés à la docker (`ID`, `Names`), `network list` et `volume list` lus en JSON (les en-têtes du tableau sont traduits).
- [ ] Tests d'intégration contre un faux `wslc` (binaire de test piloté par variable d'environnement) pour `up`, `down` et `ps`.
- [ ] Release multi-plateforme (goreleaser : `windows/amd64`, `windows/arm64`, `linux/amd64`, `linux/arm64`).
- [ ] Module path définitif (`github.com/<owner>/wslc-go`) pour `go install`.
- [ ] Port de `install.sh` (téléchargement du binaire de release au lieu de pip/uv).
