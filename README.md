# parkingapp

Telegram-Bot für Parkplätze von [fair parken](https://www.fairparken.com/de-DE/pay) mit FreeFlow (Kennzeichenerkennung, ohne Ticket).

- Mehrere Nutzer, jeder verwaltet eigene Kennzeichen
- Push-Nachricht, sobald ein Parkvorgang für ein Kennzeichen erkannt wird, und wenn er nicht mehr offen ist
- Erinnerung kurz vor Ablauf der 48-h-Frist für die Online-Zahlung
- Aktive Abfrage (`/check`) und Tarife mit Preisen für den laufenden Parkvorgang (`/tarife`)
- Bezahl-Button öffnet die offizielle Seite mit vorausgefülltem Kennzeichen – der Bot berührt keine Zahlungsdaten

## Bedienung

Alles geht über Buttons: `/start` blendet unten im Chat dauerhaft „🏠 Menü“ ein. Das Hauptmenü führt zu *Meine Kennzeichen* → Kennzeichen → Prüfen, Bezahlen, Alarme, Tarife, Rabattcode, Verlauf, Einstellungen, Umbenennen, Entfernen (mit Rückfrage). Preis- und Dauer-Alarm erreicht man direkt über „🔔 Alarme“ im Hauptmenü oder im Kennzeichen-Menü. Menüs werden an Ort und Stelle umgebaut.

Braucht der Bot eine Eingabe (Kennzeichen, Name, Rabattcode, eigener Betrag), fragt er mit einem „✖️ Abbrechen“-Button und nimmt die nächste Nachricht als Antwort. Ungültige Eingaben fragt er erneut ab. Menü-Button oder ein Befehl brechen die Frage ab, nach 15 min verfällt sie. Alarme und Ruhezeit haben Schnellauswahl-Buttons.

Admins sehen im Hauptmenü zusätzlich Nutzer, Statistik und Rundnachricht (mit Vorschau und Bestätigung).

## Befehle

Für alles gibt es auch Befehle:

| Befehl | Funktion |
|---|---|
| `/add HH-AB 123 Name` | Kennzeichen überwachen (Name optional) |
| `/list` | eigene Kennzeichen mit Buttons |
| `/check [Kennzeichen]` | jetzt prüfen; ein Kennzeichen ohne Befehl schicken geht auch |
| `/tarife [Kennzeichen \| Parkplatz-ID]` | Tarife; Preise nur bei offenem Parkvorgang |
| `/bezahlen [Kennzeichen]` | Link zur Bezahlseite |
| `/verlauf [Kennzeichen]` | letzte 10 Parkvorgänge, Kosten pro Monat (6 Monate) |
| `/rabatt CODE [Kennzeichen]` | Preis eines offenen Parkvorgangs mit Rabattcode |
| `/kassenzettel` | Link zur offiziellen Seite, um eine Vertragsstrafe mit Kassenzettel anzufechten |
| `/einstellungen [Kennzeichen]` | Pausieren, Frist-Erinnerung, jede Preiserhöhung melden (Buttons) |
| `/alarm [Kennzeichen] 10` | Meldung, wenn die Gebühr 10 € erreicht (`aus` = abschalten) |
| `/dauer [Kennzeichen] 3` | Meldung nach 3 h Parkdauer (`1,5`, `90m`, `aus`) |
| `/ruhezeit 22-7` | Meldungen in der Zeit ohne Ton (`aus`) |
| `/remove [Kennzeichen]` | Kennzeichen entfernen |
| `/feedback [Text]` | Feedback schicken |
| `/users`, `/allow ID`, `/block ID`, `/stats`, `/nutzung 30`, `/rundnachricht Text`, `/karenz 15` | nur Admins, siehe [Zugang](#zugang) |

Der Verlauf zeigt nur Parkvorgänge, die der Bot gesehen hat, nachdem der Nutzer das Kennzeichen hinzugefügt hat. Als Gebühr gilt der zuletzt gesehene Betrag.
Pausierte Kennzeichen werden nicht abgefragt, solange niemand sonst sie aktiv überwacht.

## Betrieb

```sh
cp .env.example .env   # TELEGRAM_BOT_TOKEN eintragen (von @BotFather)
docker compose up -d --build
docker compose logs -f
```

Die SQLite-Datenbank liegt im Volume `data`. Konfiguration siehe `.env.example`.

## Feedback

Nutzer schreiben über „💬 Feedback“ (oder `/feedback Text`) und sehen unter „Mein Feedback“ Status und Antworten. Admins bekommen jedes neue Feedback mit Buttons für den Status (🆕 neu, 🔧 in Arbeit, ✅ erledigt, 🚫 abgelehnt) und „💬 Antworten“; der Nutzer wird bei jeder Änderung benachrichtigt. Übersicht im Hauptmenü unter „📥 Feedback (n)“. Gespeichert in der Tabelle `feedback`.

## Nutzungsstatistik

Jede Interaktion (Befehl, Button, Eingabe) und jede verschickte Push-Nachricht wird pro Tag, Nutzer und Funktion gezählt (Tabelle `usage`, nur Zähler, keine Inhalte). Admins sehen unter „📈 Nutzung“ bzw. `/nutzung 30` aktive Nutzer (7/30 Tage) sowie Interaktionen und Nutzer je Funktion für 7, 30 oder 90 Tage; Befehle, Buttons und Eingaben derselben Funktion werden zusammengefasst, Admin-Funktionen ausgenommen.

## Zugang

- `ADMIN_USER_IDS`: Admins. Schreibt ein unbekannter Nutzer dem Bot, wird daraus eine Zugangsanfrage, und alle Admins bekommen eine Nachricht mit „Freischalten“/„Ablehnen“.
- Admin-Befehle: `/users` (Nutzer, Anfragen, Anzahl Kennzeichen, Buttons), `/allow ID`, `/block ID`. Sperren löscht die Kennzeichen des Nutzers, damit keine Pushes mehr kommen.
- `ALLOWED_USER_IDS`: Nutzer mit festem Zugang, unabhängig von der Freischaltung.
- Sind beide leer, kann jeder den Bot nutzen.

## Ausfallmeldung

Der Bot schreibt alle 30 s einen Zeitstempel in die DB. War er beim Start länger als `OFFLINE_NOTICE_AFTER` (Standard 5 min) weg, bekommen alle Nutzer eine Nachricht mit Beginn und Dauer des Ausfalls, und die in der Zeit bei Telegram aufgelaufenen Befehle werden verworfen. Nach kürzeren Neustarts (z. B. Deploy) werden sie normal abgearbeitet. Parkvorgänge aus der Ausfallzeit meldet der erste Abfragezyklus nach dem Start nach.

## Überwachung von außen

Fällt der ganze Server aus, kann der Bot das nicht melden. Dafür `HEALTHCHECK_URL` setzen (z. B. kostenloser Check bei healthchecks.io, Periode = `POLL_INTERVAL`, Grace ≈ 10 min). Der Bot pingt nach jeder Abfragerunde; schlagen alle Abfragen einer Runde fehl, ruft er `<url>/fail` auf. Healthchecks.io meldet sich dann per Mail, Telegram o. ä.

Die eigene Telegram-ID steht im Log (`access denied`), sobald man dem Bot schreibt.

Entwicklung: `go test ./...`, Live-Test gegen die echte API: `FAIRPARKEN_LIVE=1 go test ./internal/fairparken -run Live`.

## fair-parken-API

Es gibt keine offizielle API. Die Endpunkte stammen aus dem JavaScript des Web-Clients `https://pay.fairparken.com` und können sich ohne Ankündigung ändern.

| Endpunkt | Verwendung |
|---|---|
| `POST api/AnprResults/AnprResultSearch` `{"plate":"HHAB123","voucherCode":""}` | offene Parkvorgänge eines Kennzeichens, `[]` wenn keiner |
| `GET api/AnprResults/GetTariffs/{quotaID}` | Tarife (Fahrzeugklassen: `icon` = `null`/Auto, `Wohnmobil`, `Bus`, `Motorrad`) |
| `POST api/Payment/ApplyVoucherCode` (Parkvorgang mit `tariffId`) | Preis für einen Tarif neu berechnen; nutzt der Web-Client bei der Tarifwahl. Der Bot stellt danach den ursprünglichen Tarif wieder her. |
| `https://pay.fairparken.com/plate/{Kennzeichen}` | Bezahlseite mit vorausgefülltem Kennzeichen |

Felder eines Treffers (`anprResult`): `plate`, `areaGkz`, `quotaID`, `quotaAddress`, `tariffId`, `realUTC_IN`, `realUTC_OUT`, `price`, `originalPrice`, `voucherValue`, `orderType`.

Weitere Endpunkte:

| Endpunkt | Verwendung |
|---|---|
| `ApplyVoucherCode` mit `voucherCode` | Preis mit Rabattcode (`/rabatt`); danach setzt der Bot den Code wieder zurück |
| `articles` im Suchergebnis | Zusatzprodukte wie Tagesgästekarten, `price.amount` in Cent (laut Mock-Daten im Web-Client) |
| `https://kassenzettel.fairparken.com` (`api/FileNumber`, `api/UploadReceiptImage`, `api/SubmitForm`) | Vertragsstrafe anfechten; braucht reCAPTCHA, daher nur verlinkt |
| `https://pay.fairparken.at` | gleiche API für Österreich, nicht eingebunden |

Beobachtet an einem echten Parkvorgang (Kaufland Hamburg, 11/2026):

- Der Parkvorgang erscheint schon während des Parkens in der Suche (Push ca. 1–4 min nach der Einfahrt).
- Solange das Auto steht, läuft `realUTC_OUT` minutengenau mit der aktuellen Zeit mit. Nach der Ausfahrt bleibt es stehen.
- `price` ist in Euro und der Preis **bei Zahlung jetzt**, `paidUntil` die Zeit, bis zu der er gilt (z. B. 3,00 € für die erste Stunde) – auch innerhalb einer Freiparkzeit.
- Eine kostenlose Ausfahrt (Freiparkzeit) ist nicht als solche erkennbar: der Vorgang verschwindet einfach.
- Weitere Felder: `quotaAddition` (z. B. „Kaufland“), `hasVoucher` (vermutlich: Parkplatz nimmt Rabattcodes an), `isResult`, `readingId`, `subParkings`.

Daraus leitet der Bot ab (Schwelle 3 min zwischen Abfragezeit und `realUTC_OUT`):

| Beobachtung | Bedeutung | Meldung |
|---|---|---|
| Vorgang da, `realUTC_OUT` ≈ jetzt | parkt | „Neuer Parkvorgang“ mit „Bei Zahlung jetzt“ |
| Vorgang da, `realUTC_OUT` steht | ausgefahren, Gebühr offen | „Ausfahrt erkannt“ mit offenem Betrag und Frist |
| verschwindet, während er noch „parkt“, Parkdauer ≤ Karenzzeit | kostenlos ausgefahren | „kostenlos“ |
| verschwindet, während er noch „parkt“, Parkdauer > Karenzzeit | vor der Ausfahrt bezahlt (Automat/online) | „vermutlich vor der Ausfahrt bezahlt“, zählt mit zuletzt angezeigtem Preis in die Kosten |
| verschwindet, Karenzzeit liegt in der Spanne der möglichen Parkdauer | nicht entscheidbar | „kostenlos oder vor der Ausfahrt bezahlt“ |
| verschwindet nach „Ausfahrt erkannt“ | bezahlt | „ist bezahlt“ |

Die Ausfahrt liegt zwischen der letzten Abfrage, die das Auto noch parkend gesehen hat, und der ersten ohne Vorgang; die Parkdauer ist daher nur als Spanne bekannt.

**AGB von fair parken** (in der Web-App unter `pay.fairparken.com/assets/agb/de.json`, Ziffer 3): Ausfahrt innerhalb von 15 min nach der Einfahrt ist kein Parkvorgang und kostenlos; nach einer Zahlung hat man 15 min zum Ausfahren; das Entgelt ist eigentlich vor der Ausfahrt am Automaten zu zahlen, ergänzend online bis 48 h nach der Ausfahrt. Danach folgen Halterabfrage und Zahlungsaufforderung mit Vertragsstrafe (Höhe laut Schild, zahlbar in 14 Tagen). Die AGB verweisen für die Nutzungsbedingungen auf die Schilder vor Ort; einzelne Parkplätze können abweichen. Der Bot erinnert standardmäßig 12 h und 2 h vor Ablauf der 48 h (`REMIND_BEFORE`).

**Karenzzeit:** Admin-Einstellung im Hauptmenü („⏳ Karenzzeit“) oder `/karenz 15`, Standard aus `GRACE_PERIOD` (15 min). Sie gilt für alle Parkplätze; parkplatzspezifische Freiparkzeiten (z. B. länger mit Kassenbon) kennt der Bot nicht.

Damit eine einzelne abweichende Antwort nicht zu falschen Meldungen führt:

- „Ausfahrt erkannt“ erst, wenn `realUTC_OUT` bei zwei Abfragen im Abstand von mindestens 2 min gleich geblieben ist, und nur bei `price` > 0.
- „ohne Gebühr“/„bezahlt“ erst, wenn der Vorgang bei zwei Abfragen hintereinander fehlt (ca. 3–7 min nach der Ausfahrt).
- Taucht ein als beendet gemeldeter Vorgang wieder auf, kommt eine Korrektur („doch noch offen“), und er wird weiter überwacht.

Bezahlen vor der Ausfahrt und kostenlose Ausfahrt sind über die API selbst nicht unterscheidbar, nur über die Karenzzeit. Bisher beobachtet ist nur die kostenlose Ausfahrt; dass ein Vorgang mit Gebühr nach der Ausfahrt bis zur Zahlung (max. 48 h) in der Suche bleibt, folgt aus der Nachzahl-Möglichkeit der Website, ist aber noch nicht live gesehen.

Abfrage-Last: ein Request pro überwachtem Kennzeichen alle `POLL_INTERVAL` (Standard 3 min, mit Jitter), dazwischen `REQUEST_GAP`.
