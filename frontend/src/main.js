import {
    CheckAuth,
    ListProjects,
    ListBuckets,
    ListWiretaps,
    CreateWiretap,
    UpdateWiretap,
    DeleteWiretap,
    DefaultFields,
    ListObjectsForWiretap,
    LoadFilesNow,
    SyncWiretapNow,
    RunRetentionNow,
    Search,
    DistinctLevels,
    GetRawLine,
} from '../wailsjs/go/app/App.js';

let wiretaps = [];
let currentWiretapId = '';
let expandedRow = null;
let editingWiretapId = null; // null = creating, string = editing that wiretap
let formFields = []; // [{column, jsonKeysText, required}]
let formProjects = []; // [{id, name}] cached for the currently open form
let currentOffset = 0;
let hasMoreResults = false;
let loadingMore = false;

const el = (id) => document.getElementById(id);

// Combines a picked "YYYY-MM-DD" date with plain HH/MM/SS number-input values into a boundary — never
// a native date/time/datetime-local picker, since those render using the OS's display locale (US
// MM/DD/YYYY + AM/PM shows up regardless of the underlying value's format). A date with no time
// entered defaults to 00:00:00 for the "from" side, 23:59:59 for the "to" side.
//
// Uses Date.UTC (not the local Date constructor) so the digits the user types are taken literally —
// matching the backend, which stores each log's own wall-clock digits verbatim rather than converting
// through any timezone (see internal/parse.parseTimeString). Using the local constructor here would
// silently reinterpret "2026-07-25 09:00" as 9am in the browser's zone instead of the log's.
function dateTimeBoundary(dateValue, h, m, s, endOfDay) {
    if (!dateValue) return null;
    const [y, mo, d] = dateValue.split('-').map(Number);
    const hour = h !== '' ? Number(h) : (endOfDay ? 23 : 0);
    const minute = m !== '' ? Number(m) : (endOfDay ? 59 : 0);
    const second = s !== '' ? Number(s) : (endOfDay ? 59 : 0);
    return new Date(Date.UTC(y, mo - 1, d, hour, minute, second)).toISOString();
}

const MONTH_NAMES = ['January', 'February', 'March', 'April', 'May', 'June',
    'July', 'August', 'September', 'October', 'November', 'December'];

// A self-drawn calendar popup — always renders "YYYY-MM-DD" grid/label text ourselves, so (unlike
// native <input type="date">) nothing here depends on the OS's regional date format.
function createDatePicker(inputEl, popupEl) {
    const pad = (n) => String(n).padStart(2, '0');
    const isoOf = (y, m, d) => `${y}-${pad(m + 1)}-${pad(d)}`;

    let selected = null; // {y, m (0-based), d} or null
    let viewYear;
    let viewMonth;

    function render() {
        clearChildren(popupEl);

        const header = document.createElement('div');
        header.className = 'cal-header';
        const prevBtn = document.createElement('button');
        prevBtn.type = 'button';
        prevBtn.className = 'ghost-btn';
        prevBtn.textContent = '‹';
        prevBtn.addEventListener('mousedown', (e) => {
            e.preventDefault();
            viewMonth--;
            if (viewMonth < 0) { viewMonth = 11; viewYear--; }
            render();
        });
        const label = document.createElement('span');
        label.textContent = `${MONTH_NAMES[viewMonth]} ${viewYear}`;
        const nextBtn = document.createElement('button');
        nextBtn.type = 'button';
        nextBtn.className = 'ghost-btn';
        nextBtn.textContent = '›';
        nextBtn.addEventListener('mousedown', (e) => {
            e.preventDefault();
            viewMonth++;
            if (viewMonth > 11) { viewMonth = 0; viewYear++; }
            render();
        });
        header.append(prevBtn, label, nextBtn);
        popupEl.appendChild(header);

        const grid = document.createElement('div');
        grid.className = 'cal-grid';
        for (const dow of ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su']) {
            const c = document.createElement('div');
            c.className = 'cal-dow';
            c.textContent = dow;
            grid.appendChild(c);
        }

        const firstOfMonth = new Date(viewYear, viewMonth, 1);
        const startOffset = (firstOfMonth.getDay() + 6) % 7; // Monday-first grid
        const daysInMonth = new Date(viewYear, viewMonth + 1, 0).getDate();
        for (let i = 0; i < startOffset; i++) grid.appendChild(document.createElement('div'));
        for (let day = 1; day <= daysInMonth; day++) {
            const cell = document.createElement('div');
            cell.className = 'cal-day';
            cell.textContent = String(day);
            if (selected && selected.y === viewYear && selected.m === viewMonth && selected.d === day) {
                cell.classList.add('cal-day-selected');
            }
            cell.addEventListener('mousedown', (e) => {
                e.preventDefault();
                selected = { y: viewYear, m: viewMonth, d: day };
                inputEl.value = isoOf(viewYear, viewMonth, day);
                popupEl.hidden = true;
            });
            grid.appendChild(cell);
        }
        popupEl.appendChild(grid);
    }

    function open() {
        const base = selected ? new Date(selected.y, selected.m, selected.d) : new Date();
        viewYear = base.getFullYear();
        viewMonth = base.getMonth();
        render();
        popupEl.hidden = false;
    }

    inputEl.addEventListener('focus', open);
    inputEl.addEventListener('click', open);
    inputEl.addEventListener('blur', () => { setTimeout(() => { popupEl.hidden = true; }, 150); });

    return {
        clear() {
            selected = null;
            inputEl.value = '';
        },
    };
}

// YYYY-MM-DD HH:MM:SS, 24h, viewer's local time — never toLocaleString()'s locale-dependent US format.
// For genuine absolute instants about *this app's own activity* (when it last polled, GCS's own
// last-modified metadata) — there's no "source timezone" to preserve, so converting to the viewer's
// local time is the right call here, unlike log timestamps (see formatLogTime).
function formatIsoLocal(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const pad = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// YYYY-MM-DD HH:MM:SS, 24h, taken verbatim with zero timezone conversion — for a log line's own
// timestamp. The backend stores each log's original wall-clock digits as-is (discarding whatever
// offset the source wrote, e.g. "-06:00"), so reading it back with local getters here would
// re-introduce exactly the conversion the backend deliberately avoided. Must stay UTC getters.
function formatLogTime(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const pad = (n) => String(n).padStart(2, '0');
    return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`;
}

function formatBytes(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    const units = ['KB', 'MB', 'GB', 'TB'];
    let value = bytes / 1024;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return `${value.toFixed(1)} ${units[unit]}`;
}

function clearChildren(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
}

function cell(text, className) {
    const td = document.createElement('td');
    td.textContent = text ?? '';
    if (className) td.className = className;
    return td;
}

function currentWiretap() {
    return wiretaps.find((w) => w.id === currentWiretapId) || null;
}

function sourceLabel(w) {
    if (w.sourceType === 'gcs' && w.gcs) return `gcs: ${w.gcs.bucket}`;
    return w.sourceType;
}

function levelClass(level) {
    switch ((level || '').toUpperCase()) {
        case 'ERROR': case 'FATAL': case 'CRITICAL': return 'level-error';
        case 'WARN': case 'WARNING': return 'level-warn';
        case 'INFO': return 'level-info';
        case 'DEBUG': case 'TRACE': return 'level-debug';
        default: return 'level-other';
    }
}

// ---------- Searchable picker ----------
// A text input + a filtered dropdown list under it. `items` is [{value, label}]; onSelect(value, item)
// fires on pick. Reusable for any "search then pick one" control (GCP project, GCS bucket, ...).
function createSearchablePicker(inputEl, listEl, onSelect) {
    let items = [];
    let selectedValue = '';

    function render(filterText) {
        clearChildren(listEl);
        const needle = (filterText || '').toLowerCase();
        const matches = items.filter((it) => it.label.toLowerCase().includes(needle));
        if (matches.length === 0) {
            const empty = document.createElement('div');
            empty.className = 'picker-empty';
            empty.textContent = 'No matches';
            listEl.appendChild(empty);
        } else {
            for (const it of matches.slice(0, 50)) {
                const row = document.createElement('div');
                row.className = 'picker-item';
                row.textContent = it.label;
                row.addEventListener('mousedown', (e) => {
                    e.preventDefault();
                    selectedValue = it.value;
                    inputEl.value = it.label;
                    listEl.hidden = true;
                    onSelect(it.value, it);
                });
                listEl.appendChild(row);
            }
        }
        listEl.hidden = false;
    }

    inputEl.addEventListener('input', () => {
        selectedValue = '';
        render(inputEl.value);
    });
    inputEl.addEventListener('focus', () => { if (!inputEl.disabled) render(inputEl.value); });
    inputEl.addEventListener('blur', () => { setTimeout(() => { listEl.hidden = true; }, 150); });
    inputEl.addEventListener('keydown', (e) => { if (e.key === 'Escape') listEl.hidden = true; });

    return {
        setItems(newItems) {
            items = newItems;
        },
        setValue(value) {
            selectedValue = value;
            const match = items.find((it) => it.value === value);
            inputEl.value = match ? match.label : value;
        },
        value() {
            return selectedValue;
        },
        clear() {
            selectedValue = '';
            inputEl.value = '';
        },
    };
}

let projectPicker;
let bucketPicker;
let fromDatePicker;
let toDatePicker;

// ---------- View switching ----------

function showView(name) {
    el('view-search').hidden = name !== 'search';
    el('view-wiretaps').hidden = name !== 'wiretaps';
    el('tab-search').classList.toggle('active', name === 'search');
    el('tab-wiretaps').classList.toggle('active', name === 'wiretaps');
    if (name === 'wiretaps') renderWiretapsTable();
}

// ---------- Auth ----------

async function initAuth() {
    const pill = el('auth-pill');
    try {
        const status = await CheckAuth();
        if (status.available) {
            // Credentials are fine — nothing actionable to show, so stay out of the way.
            pill.hidden = true;
        } else {
            pill.hidden = false;
            pill.className = 'pill pill-error';
            pill.replaceChildren(document.createTextNode(status.message + ' '));
            const btn = document.createElement('button');
            btn.textContent = 'Retry';
            btn.className = 'ghost-btn';
            btn.addEventListener('click', initAuth);
            pill.appendChild(btn);
        }
    } catch (err) {
        pill.hidden = false;
        pill.className = 'pill pill-error';
        pill.textContent = `Auth check failed: ${err}`;
    }
    await refreshWiretaps();
}

// ---------- Wiretaps: shared data ----------

async function refreshWiretaps() {
    try {
        wiretaps = await ListWiretaps() || [];
    } catch {
        wiretaps = [];
    }
    renderWiretapSelect();
    if (!el('view-wiretaps').hidden) renderWiretapsTable();
}

function renderWiretapSelect() {
    const select = el('wiretap-select');
    const previous = select.value;
    clearChildren(select);
    select.appendChild(new Option('Select a wiretap…', ''));
    for (const w of wiretaps) select.appendChild(new Option(w.name, w.id));
    if (previous && wiretaps.some((w) => w.id === previous)) {
        select.value = previous;
        currentWiretapId = previous;
    }
}

// ---------- Search view ----------

async function onWiretapChange() {
    currentWiretapId = el('wiretap-select').value;
    const has = Boolean(currentWiretapId);
    el('search-main').hidden = !has;
    el('wiretap-empty-state').hidden = has;
    if (!has) return;

    collapseExpandedRow();
    el('results-table').querySelector('tbody').replaceChildren();
    el('search-status').textContent = '';
    el('scroll-status').textContent = '';
    currentOffset = 0;
    hasMoreResults = false;
    renderAdvancedFilters();
    await refreshLevelOptions();
}

function renderAdvancedFilters() {
    const container = el('advanced-filters-row');
    clearChildren(container);
    const w = currentWiretap();
    if (!w) return;

    for (const f of w.fields) {
        if (f.column === 'time' || f.column === 'level') continue;
        const label = document.createElement('label');
        label.textContent = `${f.column} contains `;
        const input = document.createElement('input');
        input.type = 'text';
        input.dataset.field = f.column;
        label.appendChild(input);
        container.appendChild(label);
    }
}

async function refreshLevelOptions() {
    const select = el('filter-level');
    const previous = select.value;
    clearChildren(select);
    select.appendChild(new Option('All levels', ''));
    try {
        const levels = await DistinctLevels(currentWiretapId) || [];
        for (const l of levels) select.appendChild(new Option(l, l));
    } catch {
        // no rows loaded yet for this wiretap — leave just "All levels"
    }
    if (previous && Array.from(select.options).some((o) => o.value === previous)) {
        select.value = previous;
    }
}

function buildFilters() {
    const fields = {};
    for (const input of el('advanced-filters-row').querySelectorAll('input[data-field]')) {
        if (input.value) fields[input.dataset.field] = input.value;
    }
    return {
        timeFrom: dateTimeBoundary(
            el('filter-from-date').value, el('filter-from-h').value, el('filter-from-m').value, el('filter-from-s').value, false),
        timeTo: dateTimeBoundary(
            el('filter-to-date').value, el('filter-to-h').value, el('filter-to-m').value, el('filter-to-s').value, true),
        level: el('filter-level').value,
        fields,
        text: el('query-input').value,
        offset: currentOffset,
    };
}

function appendResultRows(rows, tbody) {
    for (const row of rows) {
        const tr = document.createElement('tr');
        tr.classList.add('result-row');
        tr.appendChild(cell(formatLogTime(row.time), 'col-time'));
        const levelTd = document.createElement('td');
        levelTd.className = 'col-level';
        if (row.level) {
            const badge = document.createElement('span');
            badge.className = `level-badge ${levelClass(row.level)}`;
            badge.textContent = row.level;
            levelTd.appendChild(badge);
        }
        tr.appendChild(levelTd);
        tr.appendChild(cell((row.fields || {}).msg, 'col-msg'));
        tr.appendChild(cell(`${row.sourceFile}:${row.sourceLine}`, 'col-source'));
        tr.addEventListener('click', () => toggleExpandRow(tr, row));
        tbody.appendChild(tr);
    }
}

function updateScrollStatus() {
    const statusEl = el('scroll-status');
    if (loadingMore) {
        statusEl.textContent = 'Loading more…';
    } else if (hasMoreResults) {
        statusEl.textContent = 'Scroll for more…';
    } else if (currentOffset > 0) {
        statusEl.textContent = 'End of results.';
    } else {
        statusEl.textContent = '';
    }
}

// Newest-first, unbounded scroll: each fetch is one page (Filters.offset/store.PageSize), appended to
// what's already on screen rather than replacing it — no page-number UI, just keep scrolling.
async function onSearch() {
    currentOffset = 0;
    hasMoreResults = false;
    collapseExpandedRow();
    const statusEl = el('search-status');
    const tbody = el('results-table').querySelector('tbody');
    clearChildren(tbody);
    statusEl.textContent = 'Searching…';
    el('scroll-status').textContent = '';

    try {
        const result = await Search(currentWiretapId, buildFilters());
        const rows = result.rows || [];
        hasMoreResults = Boolean(result.hasMore);
        currentOffset = rows.length;
        statusEl.textContent = `${rows.length} row(s) loaded`;
        appendResultRows(rows, tbody);
        updateScrollStatus();
    } catch (err) {
        statusEl.textContent = `Search failed: ${err}`;
    }
}

async function loadMoreResults() {
    if (loadingMore || !hasMoreResults || !currentWiretapId) return;
    loadingMore = true;
    updateScrollStatus();
    try {
        const filters = buildFilters();
        filters.offset = currentOffset;
        const result = await Search(currentWiretapId, filters);
        const rows = result.rows || [];
        hasMoreResults = Boolean(result.hasMore);
        appendResultRows(rows, el('results-table').querySelector('tbody'));
        currentOffset += rows.length;
        el('search-status').textContent = `${currentOffset} row(s) loaded`;
    } catch (err) {
        el('scroll-status').textContent = `Failed to load more: ${err}`;
    } finally {
        loadingMore = false;
        updateScrollStatus();
    }
}

function onWindowScroll() {
    if (el('search-main').hidden) return;
    const nearBottom = window.innerHeight + window.scrollY >= document.body.offsetHeight - 200;
    if (nearBottom) loadMoreResults();
}

function onResetFilters() {
    el('query-input').value = '';
    fromDatePicker.clear();
    toDatePicker.clear();
    el('filter-from-h').value = '';
    el('filter-from-m').value = '';
    el('filter-from-s').value = '';
    el('filter-to-h').value = '';
    el('filter-to-m').value = '';
    el('filter-to-s').value = '';
    el('filter-level').value = '';
    for (const input of el('advanced-filters-row').querySelectorAll('input[data-field]')) {
        input.value = '';
    }
    onSearch();
}

function collapseExpandedRow() {
    if (expandedRow) {
        expandedRow.remove();
        expandedRow = null;
    }
}

async function toggleExpandRow(tr, row) {
    if (expandedRow && expandedRow.dataset.forRow === row.fileHash) {
        collapseExpandedRow();
        return;
    }
    collapseExpandedRow();

    const detailTr = document.createElement('tr');
    detailTr.className = 'detail-row';
    detailTr.dataset.forRow = row.fileHash;
    const td = document.createElement('td');
    td.colSpan = 4;
    td.textContent = 'Loading…';
    detailTr.appendChild(td);
    tr.after(detailTr);
    expandedRow = detailTr;

    try {
        const raw = await GetRawLine(currentWiretapId, row.fileHash);
        td.replaceChildren(buildDetailView(raw));
    } catch (err) {
        td.textContent = `Failed to load raw line: ${err}`;
    }
}

function buildDetailView(raw) {
    const wrapper = document.createElement('div');
    wrapper.className = 'detail';

    let parsed = null;
    try {
        parsed = JSON.parse(raw);
    } catch {
        // not an object we can walk field-by-field — fall through to raw-only view
    }

    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        const fieldsTable = document.createElement('table');
        fieldsTable.className = 'fields-table';
        for (const [key, value] of Object.entries(parsed)) {
            const tr = document.createElement('tr');

            const keyTd = document.createElement('td');
            keyTd.className = 'field-key';
            keyTd.textContent = key;

            const valueTd = document.createElement('td');
            valueTd.className = 'field-value';
            const valueText = typeof value === 'string' ? value : JSON.stringify(value);
            valueTd.textContent = valueText;

            const filterTd = document.createElement('td');
            const filterBtn = document.createElement('button');
            filterBtn.className = 'ghost-btn field-filter-btn';
            filterBtn.title = `Search for ${key}: ${valueText}`;
            filterBtn.textContent = '🔍';
            filterBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                el('query-input').value = valueText;
                onSearch();
            });
            filterTd.appendChild(filterBtn);

            tr.appendChild(keyTd);
            tr.appendChild(valueTd);
            tr.appendChild(filterTd);
            fieldsTable.appendChild(tr);
        }
        wrapper.appendChild(fieldsTable);
    }

    const rawHeader = document.createElement('div');
    rawHeader.className = 'raw-header';
    rawHeader.textContent = 'Raw line';
    wrapper.appendChild(rawHeader);

    const pre = document.createElement('pre');
    pre.className = 'raw-json';
    pre.textContent = parsed ? JSON.stringify(parsed, null, 2) : raw;
    wrapper.appendChild(pre);

    return wrapper;
}

// ---------- Wiretaps view: list ----------
// Native window.alert()/confirm() can wedge the webview's script thread (confirmed: it blocked
// Search/etc. mid-session) — everything here uses the inline status line and a two-click arm/confirm
// on Delete instead.

let pendingDeleteId = null;

function setWiretapsStatus(text) {
    el('wiretaps-status').textContent = text;
}

function renderWiretapsTable() {
    const tbody = el('wiretaps-table').querySelector('tbody');
    clearChildren(tbody);

    for (const w of wiretaps) {
        const tr = document.createElement('tr');
        tr.appendChild(cell(w.name));
        tr.appendChild(cell(sourceLabel(w)));
        tr.appendChild(cell(w.prefix));
        tr.appendChild(cell(w.retentionDays > 0 ? `${w.retentionDays}d` : 'disabled'));
        tr.appendChild(cell(w.autoLoadEnabled ? `every ${w.pollIntervalMinutes}m` : 'off'));
        tr.appendChild(cell(w.lastPolledAt ? formatIsoLocal(w.lastPolledAt) : 'never'));

        const actionsTd = document.createElement('td');
        actionsTd.className = 'row-actions';
        actionsTd.appendChild(actionButton('Load now', () => onSyncWiretapNow(w.id)));
        actionsTd.appendChild(actionButton('Run retention', () => onRunRetentionNow(w.id)));
        actionsTd.appendChild(actionButton('Edit', () => openWiretapForm(w.id)));

        const isPendingDelete = pendingDeleteId === w.id;
        const deleteBtn = actionButton(isPendingDelete ? 'Confirm delete?' : 'Delete', () => onDeleteWiretap(w.id));
        if (isPendingDelete) deleteBtn.classList.add('confirm-danger');
        actionsTd.appendChild(deleteBtn);
        tr.appendChild(actionsTd);

        tbody.appendChild(tr);
    }
}

function actionButton(label, handler) {
    const btn = document.createElement('button');
    btn.className = 'ghost-btn';
    btn.textContent = label;
    btn.addEventListener('click', handler);
    return btn;
}

async function onSyncWiretapNow(id) {
    pendingDeleteId = null;
    setWiretapsStatus('Loading…');
    try {
        const count = await SyncWiretapNow(id);
        setWiretapsStatus(`Loaded ${count} new file(s).`);
        await refreshWiretaps();
    } catch (err) {
        setWiretapsStatus(`Load failed: ${err}`);
    }
}

async function onRunRetentionNow(id) {
    pendingDeleteId = null;
    setWiretapsStatus('Running retention…');
    try {
        const deleted = await RunRetentionNow(id);
        setWiretapsStatus(`Deleted ${deleted} row(s).`);
    } catch (err) {
        setWiretapsStatus(`Retention failed: ${err}`);
    }
}

async function onDeleteWiretap(id) {
    if (pendingDeleteId !== id) {
        pendingDeleteId = id;
        renderWiretapsTable();
        return;
    }
    pendingDeleteId = null;
    setWiretapsStatus('Deleting…');
    try {
        await DeleteWiretap(id);
        setWiretapsStatus('Wiretap deleted.');
        await refreshWiretaps();
    } catch (err) {
        setWiretapsStatus(`Delete failed: ${err}`);
    }
}

// ---------- Wiretaps view: create/edit form ----------

async function openWiretapForm(wiretapId) {
    editingWiretapId = wiretapId || null;
    el('wiretap-form-error').textContent = '';
    el('wiretap-form-panel').hidden = false;
    el('wiretap-form-title').textContent = wiretapId ? 'Edit Wiretap' : 'New Wiretap';
    el('wf-files-tbody').replaceChildren();
    el('wf-load-summary').textContent = '';
    el('wf-load-files-btn').disabled = true;

    await populateProjectPicker();

    if (wiretapId) {
        const w = wiretaps.find((x) => x.id === wiretapId);
        el('wf-name').value = w.name;
        el('wf-name').disabled = true;
        el('wf-source-type').value = w.sourceType;
        el('wf-source-type').disabled = true;
        el('wf-prefix').value = w.prefix;
        el('wf-retention').value = w.retentionDays;
        el('wf-load-days-back').value = w.loadDaysBack;
        el('wf-autoload').checked = w.autoLoadEnabled;
        el('wf-poll-interval').value = w.pollIntervalMinutes;
        formFields = w.fields.map((f) => ({ column: f.column, jsonKeysText: f.jsonKeys.join(', '), required: f.required }));

        if (w.sourceType === 'gcs' && w.gcs) {
            projectPicker.setValue(w.gcs.projectId);
            el('wf-bucket-input').disabled = false;
            await populateBucketPicker(w.gcs.projectId);
            bucketPicker.setValue(w.gcs.bucket);
        }
    } else {
        el('wf-name').value = '';
        el('wf-name').disabled = false;
        el('wf-source-type').value = 'gcs';
        el('wf-source-type').disabled = false;
        el('wf-prefix').value = '';
        el('wf-retention').value = 0;
        el('wf-load-days-back').value = 0;
        el('wf-autoload').checked = false;
        el('wf-poll-interval').value = 15;
        projectPicker.clear();
        bucketPicker.clear();
        bucketPicker.setItems([]);
        el('wf-bucket-input').disabled = true;
        try {
            const defaults = await DefaultFields();
            formFields = defaults.map((f) => ({ column: f.column, jsonKeysText: f.jsonKeys.join(', '), required: f.required }));
        } catch {
            formFields = [];
        }
    }
    renderFieldsEditor();
}

function closeWiretapForm() {
    editingWiretapId = null;
    el('wiretap-form-panel').hidden = true;
}

async function populateProjectPicker() {
    try {
        formProjects = await ListProjects() || [];
    } catch {
        formProjects = [];
    }
    projectPicker.setItems(formProjects.map((p) => ({ value: p.id, label: `${p.name} (${p.id})` })));
}

async function populateBucketPicker(projectId) {
    if (!projectId) {
        bucketPicker.setItems([]);
        return;
    }
    try {
        const buckets = await ListBuckets(projectId) || [];
        bucketPicker.setItems(buckets.map((b) => ({ value: b.name, label: b.name })));
    } catch {
        bucketPicker.setItems([]);
    }
}

async function onProjectSelected(projectId) {
    bucketPicker.clear();
    el('wf-bucket-input').disabled = !projectId;
    await populateBucketPicker(projectId);
}

function renderFieldsEditor() {
    const tbody = el('wf-fields-tbody');
    clearChildren(tbody);

    formFields.forEach((f, i) => {
        const tr = document.createElement('tr');

        const colTd = document.createElement('td');
        const colInput = document.createElement('input');
        colInput.type = 'text';
        colInput.value = f.column;
        colInput.disabled = f.required;
        colInput.addEventListener('input', () => { f.column = colInput.value; });
        colTd.appendChild(colInput);

        const keysTd = document.createElement('td');
        const keysInput = document.createElement('input');
        keysInput.type = 'text';
        keysInput.value = f.jsonKeysText;
        keysInput.placeholder = 'e.g. time, :time';
        keysInput.addEventListener('input', () => { f.jsonKeysText = keysInput.value; });
        keysTd.appendChild(keysInput);

        const actionTd = document.createElement('td');
        if (!f.required) {
            const removeBtn = document.createElement('button');
            removeBtn.className = 'ghost-btn';
            removeBtn.textContent = '×';
            removeBtn.addEventListener('click', () => {
                formFields.splice(i, 1);
                renderFieldsEditor();
            });
            actionTd.appendChild(removeBtn);
        } else {
            actionTd.textContent = 'required';
            actionTd.className = 'field-required-note';
        }

        tr.appendChild(colTd);
        tr.appendChild(keysTd);
        tr.appendChild(actionTd);
        tbody.appendChild(tr);
    });
}

function onAddField() {
    formFields.push({ column: '', jsonKeysText: '', required: false });
    renderFieldsEditor();
}

function fieldsFromForm() {
    return formFields.map((f) => ({
        column: f.column.trim(),
        jsonKeys: f.jsonKeysText.split(',').map((k) => k.trim()).filter(Boolean),
        required: f.required,
    }));
}

async function onSaveWiretap() {
    const errorEl = el('wiretap-form-error');
    errorEl.textContent = '';

    const sourceType = el('wf-source-type').value;
    const input = {
        name: el('wf-name').value.trim(),
        sourceType,
        prefix: el('wf-prefix').value,
        fields: fieldsFromForm(),
        retentionDays: parseInt(el('wf-retention').value, 10) || 0,
        loadDaysBack: parseInt(el('wf-load-days-back').value, 10) || 0,
        autoLoadEnabled: el('wf-autoload').checked,
        pollIntervalMinutes: parseInt(el('wf-poll-interval').value, 10) || 15,
    };
    if (sourceType === 'gcs') {
        input.gcs = { projectId: projectPicker.value(), bucket: bucketPicker.value() };
    }

    try {
        if (editingWiretapId) {
            await UpdateWiretap(editingWiretapId, input);
        } else {
            await CreateWiretap(input);
        }
        closeWiretapForm();
        await refreshWiretaps();
    } catch (err) {
        errorEl.textContent = `Save failed: ${err}`;
    }
}

async function onFindFilesInForm() {
    if (!editingWiretapId) {
        el('wf-load-summary').textContent = 'Save the wiretap first, then load files from here.';
        return;
    }
    const tbody = el('wf-files-tbody');
    clearChildren(tbody);
    el('wf-load-summary').textContent = '';

    let files;
    try {
        files = await ListObjectsForWiretap(editingWiretapId, el('wf-browse-prefix').value) || [];
    } catch (err) {
        el('wf-load-summary').textContent = `Failed to list objects: ${err}`;
        return;
    }

    for (const f of files) {
        const tr = document.createElement('tr');

        const checkTd = document.createElement('td');
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.value = f.name;
        checkbox.addEventListener('change', updateFormLoadButtonState);
        checkTd.appendChild(checkbox);

        tr.appendChild(checkTd);
        tr.appendChild(cell(f.name));
        tr.appendChild(cell(formatBytes(f.size)));
        tr.appendChild(cell(formatIsoLocal(f.lastModified)));
        tr.appendChild(cell(f.loaded ? 'Yes' : 'No'));
        tbody.appendChild(tr);
    }
    updateFormLoadButtonState();
}

function updateFormLoadButtonState() {
    const anyChecked = Array.from(el('wf-files-tbody').querySelectorAll('input[type=checkbox]'))
        .some((cb) => cb.checked);
    el('wf-load-files-btn').disabled = !anyChecked;
}

async function onLoadFilesInForm() {
    const names = Array.from(el('wf-files-tbody').querySelectorAll('input[type=checkbox]:checked'))
        .map((cb) => cb.value);
    if (names.length === 0) return;

    const summaryEl = el('wf-load-summary');
    summaryEl.textContent = 'Loading…';

    try {
        const summaries = await LoadFilesNow(editingWiretapId, names);
        summaryEl.replaceChildren();
        for (const s of summaries) {
            const line = document.createElement('div');
            line.textContent = s.error
                ? `${s.name}: failed — ${s.error}`
                : s.alreadyLoaded
                    ? `${s.name}: already loaded, skipped`
                    : `${s.name}: ${s.rowsInserted} row(s) inserted, ${s.linesSkipped} line(s) skipped`;
            summaryEl.appendChild(line);
        }
    } catch (err) {
        summaryEl.textContent = `Load failed: ${err}`;
    }

    await onFindFilesInForm();
}

// ---------- Wire up ----------

function wireUpStaticControls() {
    el('tab-search').addEventListener('click', () => showView('search'));
    el('tab-wiretaps').addEventListener('click', () => showView('wiretaps'));
    el('goto-wiretaps-btn').addEventListener('click', () => showView('wiretaps'));

    el('wiretap-select').addEventListener('change', onWiretapChange);
    el('search-btn').addEventListener('click', () => onSearch());
    el('reset-filters-btn').addEventListener('click', onResetFilters);
    el('search-main').addEventListener('keydown', (e) => { if (e.key === 'Enter') onSearch(); });
    window.addEventListener('scroll', onWindowScroll);

    el('new-wiretap-btn').addEventListener('click', () => openWiretapForm(null));
    el('wf-add-field-btn').addEventListener('click', onAddField);
    el('wf-save-btn').addEventListener('click', onSaveWiretap);
    el('wf-cancel-btn').addEventListener('click', closeWiretapForm);
    el('wf-find-files-btn').addEventListener('click', onFindFilesInForm);
    el('wf-load-files-btn').addEventListener('click', onLoadFilesInForm);

    projectPicker = createSearchablePicker(el('wf-project-input'), el('wf-project-list'), onProjectSelected);
    bucketPicker = createSearchablePicker(el('wf-bucket-input'), el('wf-bucket-list'), () => {});

    fromDatePicker = createDatePicker(el('filter-from-date'), el('filter-from-date-popup'));
    toDatePicker = createDatePicker(el('filter-to-date'), el('filter-to-date-popup'));
}

wireUpStaticControls();
initAuth();
