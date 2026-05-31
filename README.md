# Distributed Backup MVP

## Retention / Wersje Backupu

Ten sam backup mozna wrzucac wiele razy pod jedna logiczna nazwa.
Serwer zapisze kolejne wersje i zachowa tylko ostatnie `N` wersji, jezeli ustawisz retencje.

Panel web:

- `Nazwa backupu` - stala nazwa logiczna, np. `dokumenty` albo `laptop-c`;
- `Retencja` - ile ostatnich wersji trzymac, np. `5`;
- `0` wylacza automatyczne usuwanie starych wersji.

CLI:

```powershell
backupctl backup -server http://localhost:8080 -file .\dane.zip -name dokumenty -retention 5
```

Restore dalej robisz po konkretnym `file_id`, czyli mozesz odtworzyc wybrana wersje.
Chunki sa deduplikowane po hashach: jezeli dwie wersje uzywaja tego samego chunka, usuniecie starej wersji nie kasuje wspolnych danych z node'ow.

Domyslny rozmiar chunka to teraz `512 KB` (`524288` bajtow), co jest sensowniejsze dla deduplikacji wersji niz poprzednie duze chunki.
Panel pozwala wybrac: `256 KB`, `512 KB`, `1 MB`, `4 MB`, `16 MB`, `64 MB`.
Panel i `backupctl` pokazuja tez procent deduplikacji dla kazdej wersji: ile danych zostalo uzyte ponownie z juz istniejacych chunkow zamiast zapisania jako nowe.
Gorny pasek panelu pokazuje lacznie zaoszczedzone bajty i procent dla wszystkich zapisanych wersji.

> A small Go prototype of a centrally managed, chunk-based distributed backup system.

This project stores backup data across many storage nodes. The server accepts files, splits them into content-addressed chunks, distributes those chunks across nodes, keeps metadata in SQLite, and restores files by reading the required chunks back from available nodes.

The storage nodes are intentionally simple: they do not know file names, manifests, users, or backup logic. They only store, return, and delete chunks.

## English

### What It Does

- Accepts file uploads through the central server or the web UI.
- Splits files into chunks.
- Identifies every chunk by `sha256:<hash>`.
- Stores each chunk on multiple different nodes.
- Keeps metadata in SQLite.
- Tracks node capacity, free space, heartbeat, and online/offline state.
- Restores files through the central server.
- Re-replicates chunks when active copies drop below the requested replication factor.
- Prunes extra copies when old nodes come back and the system has more active copies than needed.
- Provides a simple web dashboard for management.

### Architecture

```text
User / Browser / backupctl
        |
        v
Central backup server
- web dashboard
- upload/restore API
- SQLite metadata
- chunk placement
- replication scheduler
        |
        v
Storage nodes
- store chunks
- return chunks
- delete chunks
```

### Data Model

A file is not stored as one large blob. It is split into chunks:

```text
file.bin
  chunk 0 -> sha256:...
  chunk 1 -> sha256:...
  chunk 2 -> sha256:...
```

Each chunk is stored on several nodes, for example with replication `3`:

```text
chunk 0 -> node-1, node-7, node-22
chunk 1 -> node-3, node-9, node-41
chunk 2 -> node-2, node-7, node-80
```

The server stores the manifest and locations in SQLite. Nodes store only raw chunk files.

### Components

```text
cmd/backup-server
```

Central server. It owns metadata, file upload, restore, chunk placement, replication, pruning, and the web dashboard.

```text
cmd/backup-node
```

Storage node. It exposes a tiny chunk API and periodically reports capacity/free space to the server. The amount reserved for backups is declared with `-capacity`, for example `500MB`, `10GB`, or `1TB`.

```text
cmd/backupctl
```

Optional CLI client. It talks only to the server, not directly to nodes.

### Quick Start

Run from the project directory:

```powershell
cd C:\dev\go\Backup
```

Start the server:

```powershell
go run ./cmd/backup-server -addr :8080 -db server-data\backup.db -replication-interval 30s
```

Or use a JSON config file:

```powershell
go run ./cmd/backup-server -config configs\server.json
```

Start three nodes:

```powershell
go run ./cmd/backup-node -id node-1 -addr :9001 -public-addr http://localhost:9001 -server http://localhost:8080 -storage storage-node-1 -capacity 10GB
```

Or use a JSON config file:

```powershell
go run ./cmd/backup-node -config configs\node.json
```

Command-line flags override values from the config file.

```powershell
go run ./cmd/backup-node -id node-2 -addr :9002 -public-addr http://localhost:9002 -server http://localhost:8080 -storage storage-node-2 -capacity 10GB
```

```powershell
go run ./cmd/backup-node -id node-3 -addr :9003 -public-addr http://localhost:9003 -server http://localhost:8080 -storage storage-node-3 -capacity 10GB
```

Open the dashboard:

```text
http://localhost:8080/
```

### CLI Usage

Create a backup:

```powershell
go run ./cmd/backupctl backup -server http://localhost:8080 -file .\file.bin -chunk-size 1048576 -replication 3
```

Restore a file:

```powershell
go run ./cmd/backupctl restore -server http://localhost:8080 -id <file_id> -out .\restore.bin
```

### Web Dashboard

The dashboard lets you:

- view connected nodes,
- see free and used storage,
- upload files,
- choose chunk size and replication count,
- view backup manifests,
- view chunk availability as a torrent-like piece map,
- change the requested replication count for an existing backup,
- download restored files,
- delete backups.

Chunk map colors:

- green: healthy, enough active copies;
- yellow: degraded, fewer active copies than requested;
- red: missing, no active copy is currently available;
- blue: extra, more active copies than requested.

### SQLite Metadata

By default, server metadata is stored in:

```text
server-data/backup.db
```

SQLite stores:

- nodes,
- backup manifests,
- file chunks,
- chunk locations,
- requested replication factor.

Actual chunk data is not stored in SQLite. Chunk files live inside node storage directories such as:

```text
storage-node-1/
storage-node-2/
storage-node-3/
```

### Node Failures And Replication

Nodes send a heartbeat roughly every 15 seconds. If the server does not see a node for about 45 seconds, that node is treated as offline.

If a backup requires 3 copies and 2 nodes disappear, restore can still work as long as at least one active copy of every chunk is available.

The replication scheduler runs in the background:

- if active copies are below the requested count, it creates new copies on other active nodes;
- if old nodes return and there are too many active copies, it removes extra copies;
- if no active copy of a chunk exists, the server must wait until at least one node with that chunk comes back.

### Server API

```text
GET  /nodes
DELETE /nodes/{id}
GET  /files
POST /backups?chunk_size=524288&replication=3
GET  /files/{id}/manifest
GET  /files/{id}/availability
PATCH /files/{id}/replication
GET  /files/{id}/download
DELETE /files/{id}
GET  /chunks/{hash}/locations
```

### Node API

```text
PUT    /chunks/{hash}
GET    /chunks/{hash}
DELETE /chunks/{hash}
DELETE /storage?confirm=delete-all-chunks
GET    /health
```

To remove an online node and clear its local storage through the server, use the dashboard action `Wyczysc i usun` or:

```text
DELETE /nodes/{id}?purge=true
```

If the node is offline, the server can only remove its metadata. The physical storage directory must be cleaned on that machine, for example:

```powershell
go run ./cmd/backup-node -storage storage-node-1 -wipe-storage
```

### Current Limitations

- No encryption yet.
- No authentication yet.
- Not intended to be exposed directly to the public internet.
- The scheduler is simple and should be hardened before production use.

---

# Rozproszony Backup MVP

> Mały prototyp w Go: centralnie zarządzany, chunkowy system rozproszonego backupu.

Projekt zapisuje dane backupu na wielu node’ach magazynujących. Serwer przyjmuje pliki, dzieli je na chunki, rozsyła chunki po node’ach, zapisuje metadane w SQLite i odtwarza pliki, pobierając potrzebne chunki z dostępnych node’ów.

Node’y są celowo proste: nie znają nazw plików, manifestów, użytkowników ani logiki backupu. Przechowują tylko chunki.

## Co To Robi

- Przyjmuje pliki przez centralny serwer albo panel WWW.
- Dzieli pliki na chunki.
- Identyfikuje każdy chunk przez `sha256:<hash>`.
- Zapisuje każdy chunk na kilku różnych node’ach.
- Trzyma metadane w SQLite.
- Śledzi pojemność node’ów, wolne miejsce, heartbeat i status online/offline.
- Odtwarza pliki przez centralny serwer.
- Dorabia brakujące kopie chunków, gdy aktywnych kopii jest za mało.
- Usuwa nadmiarowe kopie, gdy stare node’y wrócą i kopii jest za dużo.
- Udostępnia prosty panel WWW do zarządzania.

## Architektura

```text
Użytkownik / przeglądarka / backupctl
        |
        v
Centralny serwer backupu
- panel WWW
- API upload/restore
- metadane SQLite
- wybór node'ów
- scheduler replikacji
        |
        v
Node'y magazynujące
- zapis chunków
- odczyt chunków
- usuwanie chunków
```

## Model Danych

Plik nie jest przechowywany jako jeden duży blob. Jest dzielony na chunki:

```text
file.bin
  chunk 0 -> sha256:...
  chunk 1 -> sha256:...
  chunk 2 -> sha256:...
```

Każdy chunk jest przechowywany na kilku node’ach. Przykład dla replikacji `3`:

```text
chunk 0 -> node-1, node-7, node-22
chunk 1 -> node-3, node-9, node-41
chunk 2 -> node-2, node-7, node-80
```

Serwer zapisuje manifest i lokalizacje w SQLite. Node’y zapisują tylko fizyczne pliki `.chunk`.

## Komponenty

```text
cmd/backup-server
```

Centralny serwer. Zarządza metadanymi, uploadem, restore, wyborem node’ów, replikacją, przycinaniem nadmiarowych kopii i panelem WWW.

```text
cmd/backup-node
```

Node magazynujący. Ma małe API chunków i okresowo raportuje pojemność oraz wolne miejsce do serwera.

```text
cmd/backupctl
```

Opcjonalne narzędzie CLI. Łączy się tylko z serwerem, nie bezpośrednio z node’ami.

## Szybki Start

Uruchamiaj z katalogu projektu:

```powershell
cd C:\dev\go\Backup
```

Start serwera:

```powershell
go run ./cmd/backup-server -addr :8080 -db server-data\backup.db -replication-interval 30s
```

Start trzech node’ów:

```powershell
go run ./cmd/backup-node -id node-1 -addr :9001 -public-addr http://localhost:9001 -server http://localhost:8080 -storage storage-node-1
```

```powershell
go run ./cmd/backup-node -id node-2 -addr :9002 -public-addr http://localhost:9002 -server http://localhost:8080 -storage storage-node-2
```

```powershell
go run ./cmd/backup-node -id node-3 -addr :9003 -public-addr http://localhost:9003 -server http://localhost:8080 -storage storage-node-3
```

Panel WWW:

```text
http://localhost:8080/
```

## Użycie CLI

Backup pliku:

```powershell
go run ./cmd/backupctl backup -server http://localhost:8080 -file .\file.bin -chunk-size 1048576 -replication 3
```

Odtworzenie pliku:

```powershell
go run ./cmd/backupctl restore -server http://localhost:8080 -id <file_id> -out .\restore.bin
```

## Panel WWW

Panel pozwala:

- zobaczyć podłączone node’y,
- zobaczyć wolne i zajęte miejsce,
- wysłać plik do backupu,
- wybrać rozmiar chunka i liczbę kopii,
- podejrzeć manifest backupu,
- podejrzeć dostępność chunków jako mapę kawałków podobną do klientów torrent,
- zmienić wymaganą liczbę kopii istniejącego backupu,
- pobrać odtworzony plik,
- usunąć backup.

Kolory mapy chunków:

- zielony: zdrowy, jest wystarczająco aktywnych kopii;
- żółty: zdegradowany, aktywnych kopii jest mniej niż wymagano;
- czerwony: brak, nie ma żadnej aktywnej kopii;
- niebieski: nadmiar, aktywnych kopii jest więcej niż wymagano.

## SQLite

Domyślna baza metadanych serwera:

```text
server-data/backup.db
```

SQLite zapisuje:

- node’y,
- manifesty backupów,
- chunki plików,
- lokalizacje chunków,
- wymaganą liczbę kopii.

Dane chunków nie są trzymane w SQLite. Fizyczne chunki są w katalogach node’ów:

```text
storage-node-1/
storage-node-2/
storage-node-3/
```

## Awarie Node’ów I Replikacja

Node wysyła heartbeat mniej więcej co 15 sekund. Jeśli serwer nie widzi node’a przez około 45 sekund, traktuje go jako offline.

Jeśli backup wymaga 3 kopii i 2 node’y znikną, restore może nadal działać, o ile istnieje przynajmniej jedna aktywna kopia każdego chunka.

Scheduler replikacji działa w tle:

- jeśli aktywnych kopii jest za mało, dorabia kopie na innych aktywnych node’ach;
- jeśli stare node’y wrócą i aktywnych kopii jest za dużo, usuwa nadmiar;
- jeśli nie ma żadnej aktywnej kopii chunka, serwer czeka aż wróci przynajmniej jeden node z tym chunkiem.

## API Serwera

```text
GET  /nodes
DELETE /nodes/{id}
GET  /files
POST /backups?chunk_size=1048576&replication=3
GET  /files/{id}/manifest
GET  /files/{id}/availability
PATCH /files/{id}/replication
GET  /files/{id}/download
DELETE /files/{id}
GET  /chunks/{hash}/locations
```

## API Node’a

```text
PUT    /chunks/{hash}
GET    /chunks/{hash}
DELETE /chunks/{hash}
DELETE /storage?confirm=delete-all-chunks
GET    /health
```

Jeśli node działa i chcesz usunąć go razem z katalogiem storage, użyj w panelu `Wyczysc i usun` albo:

```text
DELETE /nodes/{id}?purge=true
```

Jeśli node jest offline, serwer może usunąć tylko metadane. Fizyczny katalog trzeba wyczyścić lokalnie na tej maszynie:

```powershell
go run ./cmd/backup-node -storage storage-node-1 -wipe-storage
```

## Ograniczenia

- Nie ma jeszcze szyfrowania.
- Nie ma jeszcze autoryzacji.
- Nie wystawiaj tego bezpośrednio do internetu.
- Scheduler jest prosty i przed produkcją wymaga dalszego utwardzenia.
## Config Files / Pliki Konfiguracyjne

Parametry mozna podawac dalej z linii komend albo z pliku JSON.
Flagi z linii komend maja pierwszenstwo i nadpisuja wartosci z pliku.

Server:

```powershell
go run ./cmd/backup-server -config configs\server.json
```

Node:

```powershell
go run ./cmd/backup-node -config configs\node.json
```

Przyklad nadpisania tylko jednego parametru z configu:

```powershell
go run ./cmd/backup-node -config configs\node.json -capacity 50GB
```
