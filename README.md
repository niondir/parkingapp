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
| `/users`, `/allow ID`, `/block ID`, `/stats`, `/rundnachricht Text` | nur Admins, siehe [Zugang](#zugang) |

Der Verlauf zeigt nur Parkvorgänge, die der Bot gesehen hat, nachdem der Nutzer das Kennzeichen hinzugefügt hat. Als Gebühr gilt der zuletzt gesehene Betrag.
Pausierte Kennzeichen werden nicht abgefragt, solange niemand sonst sie aktiv überwacht.

## Betrieb

```sh
cp .env.example .env   # TELEGRAM_BOT_TOKEN eintragen (von @BotFather)
docker compose up -d --build
docker compose logs -f
```

Die SQLite-Datenbank liegt im Volume `data`. Konfiguration siehe `.env.example`.

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

Offene Punkte, erst mit einem echten Parkvorgang prüfbar:

- Ob die Suche schon während des Parkens einen Treffer liefert (die Website bietet Zahlung vor der Ausfahrt an, daher wahrscheinlich).
- `price` wird als Euro interpretiert (der Web-Client rechnet `price * 100` in Cent um).
- Was `realUTC_OUT` vor der Ausfahrt enthält. Die Erinnerung greift nur, wenn die Ausfahrtszeit fest ist.

Abfrage-Last: ein Request pro überwachtem Kennzeichen alle `POLL_INTERVAL` (Standard 3 min, mit Jitter), dazwischen `REQUEST_GAP`.
