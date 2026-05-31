# Pomysl: rozproszony backup na wielu komputerach

## Idea

Zalozenie jest takie, ze mamy np. 100 komputerow. Na kazdym komputerze wydzielamy miejsce na backup, np. plik, folder albo partycje o rozmiarze 10 GB.

Lacznie daje to:

```text
100 komputerow * 10 GB = 1000 GB brutto
```

Jesli kazdy fragment danych ma miec minimum 3 kopie, to realna pojemnosc uzytkowa wynosi okolo:

```text
1000 GB / 3 = okolo 333 GB
```

Centralny serwer nie musi trzymac wszystkich danych. Jego glowna rola to zarzadzanie:

- ktore komputery sa aktywne,
- ile maja wolnego miejsca,
- gdzie znajduja sie fragmenty plikow,
- czy kazdy fragment ma minimum 3 kopie,
- kiedy trzeba dorobic brakujaca kopie,
- jak odtworzyc oryginalny plik z fragmentow.

## Najwazniejszy pomysl: dzielenie plikow na chunki

Nie warto trzymac backupu jako jednego wielkiego pliku. Lepiej dzielic pliki na mniejsze fragmenty, czyli chunki.

Przyklad dla pliku 10 GB:

```text
plik.iso 10 GB
  chunk 0: 64 MB
  chunk 1: 64 MB
  chunk 2: 64 MB
  ...
```

Kazdy chunk dostaje swoj hash, np. SHA-256:

```text
sha256(dane_chunka) = chunk_id
```

Dzieki temu:

- latwo sprawdzic, czy dane nie sa uszkodzone,
- mozna wykrywac duplikaty,
- nazwa chunka moze byc jego hashem,
- serwer wie dokladnie, ktory fragment gdzie lezy,
- uszkodzony albo brakujacy fragment mozna odtworzyc z innej kopii.

## Format przechowywania na komputerach

Na kazdym komputerze klient backupu mialby folder storage, np.:

```text
storage/
  aa/
    aa1234abcd...chunk
  bb/
    bb9834abcd...chunk
  f1/
    f1abcd9834...chunk
```

Nazwa pliku moze byc hashem zawartosci chunka.

Sam plik chunka moze zawierac tylko czyste bajty danych. Metadane nie musza byc zapisane w kazdym chunku, bo centralny serwer trzyma je w bazie.

Prosty wariant:

```text
chunk file = czyste dane
nazwa pliku = sha256 danych
metadane = baza danych na serwerze
```

Mozna tez zrobic bardziej opisowy format chunka:

```text
MAGIC: BKCH
VERSION: 1
HASH: sha256:...
SIZE: ...
DATA: ...
```

Na start prostszy wariant jest lepszy: czyste dane + hash jako nazwa pliku.

## Manifest pliku

Oryginalny plik odtwarza sie z manifestu. Manifest opisuje, z jakich chunkow sklada sie plik i w jakiej kolejnosci trzeba je polaczyc.

Przyklad manifestu:

```json
{
  "file_id": "abc123",
  "name": "plik.iso",
  "size": 10737418240,
  "chunk_size": 67108864,
  "chunks": [
    {
      "index": 0,
      "hash": "sha256:aaa...",
      "size": 67108864
    },
    {
      "index": 1,
      "hash": "sha256:bbb...",
      "size": 67108864
    }
  ]
}
```

Serwer dodatkowo wie, na ktorych komputerach lezy kazdy chunk:

```json
{
  "chunk": "sha256:aaa...",
  "copies": ["pc-12", "pc-41", "pc-77"]
}
```

## Rola centralnego serwera

Centralny serwer powinien miec baze danych z informacjami o komputerach, plikach, chunkach i lokalizacjach chunkow.

Przykladowe tabele:

```text
nodes
- id
- name
- address
- free_space
- used_space
- last_seen
- status

files
- id
- name
- size
- created_at
- owner

chunks
- id/hash
- size
- created_at

file_chunks
- file_id
- chunk_id
- chunk_index

chunk_locations
- chunk_id
- node_id
- verified_at
- status
```

Serwer pilnuje, zeby kazdy chunk mial minimum 3 kopie na roznych komputerach.

Jesli komputer zniknie z sieci albo padnie, serwer sprawdza, ktore chunki stracily kopie. Potem zleca innym komputerom dorobienie brakujacych kopii.

## Rola klienta na kazdym komputerze

Na kazdym komputerze dziala maly program-klient.

Klient powinien:

- miec lokalny folder storage,
- przyjmowac chunki od serwera albo innych klientow,
- wysylac chunki do innych komputerow,
- sprawdzac hashe zapisanych chunkow,
- raportowac wolne miejsce,
- wysylac heartbeat do serwera,
- usuwac chunki tylko wtedy, gdy serwer na to pozwoli.

## Przykladowy przeplyw dodawania pliku

1. Uzytkownik dodaje plik do backupu.
2. Program dzieli plik na chunki, np. po 64 MB.
3. Dla kazdego chunka liczony jest SHA-256.
4. Serwer sprawdza, czy taki chunk juz istnieje.
5. Jesli chunk juz istnieje, nie trzeba go wysylac drugi raz.
6. Jesli chunk jest nowy, serwer wybiera 3 komputery docelowe.
7. Chunk trafia na 3 rozne komputery.
8. Kazdy komputer potwierdza zapis i poprawny hash.
9. Serwer zapisuje w bazie, gdzie sa kopie.
10. Manifest pliku zostaje zapisany w bazie.

## Przykladowy przeplyw odtwarzania pliku

1. Uzytkownik wybiera plik do odtworzenia.
2. Serwer pobiera manifest pliku.
3. Dla kazdego chunka serwer wybiera komputer, ktory ma jego kopie.
4. Klient pobiera chunki.
5. Kazdy chunk jest sprawdzany po hashu.
6. Chunki sa laczone w oryginalnej kolejnosci.
7. Powstaje oryginalny plik.

## Co gdy komputer padnie

Jesli komputer nie wysyla heartbeat przez okreslony czas, np. 10 minut albo 1 godzine, serwer oznacza go jako niedostepny.

Potem serwer sprawdza:

```text
czy kazdy chunk nadal ma minimum 3 aktywne kopie?
```

Jesli jakis chunk ma tylko 2 kopie, serwer zleca utworzenie kolejnej kopii na innym komputerze.

Przyklad:

```text
chunk A byl na: pc-1, pc-2, pc-3
pc-2 padl
zostaje: pc-1, pc-3
serwer kopiuje chunk A na pc-8
nowy stan: pc-1, pc-3, pc-8
```

## Dedykowany plik 10 GB czy zwykly folder?

Sa dwa warianty.

### Wariant 1: folder storage

Najprostsze rozwiazanie:

```text
C:\BackupNodeStorage\
```

Klient pilnuje, zeby nie przekroczyc limitu, np. 10 GB.

Plusy:

- proste,
- latwe do debugowania,
- widac pliki chunkow,
- latwo testowac.

Minusy:

- trzeba dobrze pilnowac limitu miejsca,
- system plikow moze miec bardzo duzo malych plikow.

### Wariant 2: jeden duzy plik-kontener 10 GB

Na kazdym komputerze tworzymy plik:

```text
backup-node.dat 10 GB
```

W srodku tego pliku klient sam zarzadza blokami.

Plusy:

- jeden plik na dysku,
- latwiej ograniczyc miejsce,
- mozna miec wlasny prosty format.

Minusy:

- trudniejsze programowanie,
- trzeba zrobic wlasny indeks wolnych blokow,
- trudniej odzyskiwac dane recznie,
- wieksze ryzyko bledu w pierwszej wersji.

Na start lepszy jest folder storage. Plik-kontener mozna zrobic pozniej.

## Proponowany stack techniczny

Dobry prosty zestaw:

```text
Serwer:
- Go
- SQLite na start albo PostgreSQL pozniej
- HTTP albo gRPC API
- scheduler replikacji

Klient:
- Go
- lokalny folder storage
- heartbeat do serwera
- upload/download chunkow
- weryfikacja SHA-256
```

## Minimalne API

Przykladowe endpointy serwera:

```text
POST /nodes/register
POST /nodes/heartbeat
POST /files
GET  /files/{id}/manifest
POST /chunks/plan-upload
POST /chunks/confirm
GET  /chunks/{hash}/locations
POST /replication/tasks
```

Przykladowe endpointy klienta:

```text
PUT /chunks/{hash}
GET /chunks/{hash}
POST /chunks/{hash}/verify
DELETE /chunks/{hash}
GET /status
```

## Najprostsza wersja MVP

Pierwsza dzialajaca wersja moglaby robic tylko to:

1. Serwer ma liste klientow.
2. Klient rejestruje sie w serwerze.
3. Program dzieli plik na chunki.
4. Serwer rozdziela chunki na 3 komputery.
5. Klienci zapisuja chunki w folderze storage.
6. Serwer zapisuje manifest.
7. Da sie odtworzyc plik z chunkow.
8. Serwer potrafi wykryc brakujaca kopie i dorobic ja.

## Pozniejsze ulepszenia

Mozliwe ulepszenia:

- szyfrowanie chunkow przed wyslaniem,
- kompresja,
- deduplikacja na poziomie chunkow,
- wersjonowanie plikow,
- limity per uzytkownik,
- panel WWW,
- erasure coding zamiast 3 pelnych kopii,
- priorytety replikacji,
- test integralnosci raz na jakis czas,
- automatyczne czyszczenie nieuzywanych chunkow.

## Wazna uwaga o szyfrowaniu

Jesli komputery naleza do roznych osob albo nie sa w pelni zaufane, chunki powinny byc szyfrowane przed wyslaniem.

Wtedy komputer przechowujacy chunk nie wie, co trzyma. Widzi tylko zaszyfrowane dane.

Schemat:

```text
oryginalny chunk
  -> kompresja opcjonalnie
  -> szyfrowanie
  -> hash zaszyfrowanych danych
  -> zapis na komputerach
```

Klucz szyfrujacy powinien byc po stronie wlasciciela backupu, a nie tylko na serwerze.

## Podsumowanie

Najlepsza organizacja to:

```text
plik = manifest
dane = chunki
nazwa chunka = hash
serwer = mapa, gdzie sa kopie
klient = lokalny magazyn chunkow
minimum bezpieczenstwa = 3 kopie na roznych komputerach
```

Na start warto zrobic prosto:

```text
folder storage + chunki po 64 MB + SHA-256 + SQLite/PostgreSQL + replikacja x3
```

To daje solidna baze pod rozproszony system backupu.
