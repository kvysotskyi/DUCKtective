import {
    CheckAuth,
    ListBuckets,
    ListLoadedBuckets,
    ListObjects,
    LoadFiles,
    Search,
    DistinctLevels,
    GetRawLine,
    DeleteOlderThan,
} from '../wailsjs/go/app/App.js';

const RESULT_CAP = 1000;

let currentBucket = '';
let expandedRow = null;

const el = (id) => document.getElementById(id);

function isoOrNull(datetimeLocalValue) {
    if (!datetimeLocalValue) return null;
    return new Date(datetimeLocalValue).toISOString();
}

function dateStartOfDayIsoOrNull(dateValue) {
    if (!dateValue) return null;
    return new Date(dateValue + 'T00:00:00Z').toISOString();
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

function formatTime(iso) {
    if (!iso) return '';
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
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

function levelClass(level) {
    switch ((level || '').toUpperCase()) {
        case 'ERROR':
        case 'FATAL':
        case 'CRITICAL':
            return 'level-error';
        case 'WARN':
        case 'WARNING':
            return 'level-warn';
        case 'INFO':
            return 'level-info';
        case 'DEBUG':
        case 'TRACE':
            return 'level-debug';
        default:
            return 'level-other';
    }
}

async function initAuth() {
    const pill = el('auth-pill');
    try {
        const status = await CheckAuth();
        if (status.available) {
            pill.className = 'pill pill-ok';
            pill.textContent = `${status.account || 'unknown account'} · ${status.projectId || 'no project'}`;
            await refreshBuckets();
        } else {
            pill.className = 'pill pill-error';
            pill.replaceChildren(document.createTextNode(status.message + ' '));
            const btn = document.createElement('button');
            btn.textContent = 'Retry';
            btn.className = 'ghost-btn';
            btn.addEventListener('click', initAuth);
            pill.appendChild(btn);
        }
    } catch (err) {
        pill.className = 'pill pill-error';
        pill.textContent = `Auth check failed: ${err}`;
    }
}

async function refreshBuckets() {
    const select = el('bucket-select');
    const previous = select.value;
    clearChildren(select);
    select.appendChild(new Option('Select a bucket…', ''));

    try {
        const buckets = await ListBuckets();
        for (const b of buckets) select.appendChild(new Option(b.name, b.name));
    } catch {
        try {
            const loaded = await ListLoadedBuckets();
            for (const name of loaded) select.appendChild(new Option(name, name));
        } catch {
            // nothing to fall back to either — leave just the placeholder
        }
    }

    if (previous && Array.from(select.options).some((o) => o.value === previous)) {
        select.value = previous;
    }
}

async function onBucketChange() {
    currentBucket = el('bucket-select').value;
    const hasBucket = Boolean(currentBucket);
    el('main').hidden = !hasBucket;
    el('bucket-empty-state').hidden = hasBucket;
    if (!hasBucket) return;

    collapseExpandedRow();
    el('results-table').querySelector('tbody').replaceChildren();
    el('files-tbody').replaceChildren();
    el('search-status').textContent = '';
    await refreshLevelOptions();
}

async function refreshLevelOptions() {
    const select = el('filter-level');
    const previous = select.value;
    clearChildren(select);
    select.appendChild(new Option('All levels', ''));
    try {
        const levels = await DistinctLevels(currentBucket);
        for (const l of levels) select.appendChild(new Option(l, l));
    } catch {
        // no rows loaded yet for this bucket — leave just "All levels"
    }
    if (previous && Array.from(select.options).some((o) => o.value === previous)) {
        select.value = previous;
    }
}

function buildFilters() {
    return {
        timeFrom: isoOrNull(el('filter-from').value),
        timeTo: isoOrNull(el('filter-to').value),
        level: el('filter-level').value,
        msg: el('filter-msg').value,
        topic: el('filter-topic').value,
        accession: el('filter-accession').value,
        studyUid: el('filter-studyuid').value,
        text: el('query-input').value,
    };
}

async function onSearch() {
    collapseExpandedRow();
    const statusEl = el('search-status');
    const tbody = el('results-table').querySelector('tbody');
    clearChildren(tbody);
    statusEl.textContent = 'Searching…';

    try {
        const result = await Search(currentBucket, buildFilters());
        statusEl.textContent = result.truncated
            ? `Showing first ${RESULT_CAP} rows (more match — narrow the filters to see the rest)`
            : `${result.rows.length} row(s)`;

        for (const row of result.rows) {
            const tr = document.createElement('tr');
            tr.classList.add('result-row');
            tr.appendChild(cell(formatTime(row.effectiveTs), 'col-time'));
            const levelTd = document.createElement('td');
            levelTd.className = 'col-level';
            if (row.level) {
                const badge = document.createElement('span');
                badge.className = `level-badge ${levelClass(row.level)}`;
                badge.textContent = row.level;
                levelTd.appendChild(badge);
            }
            tr.appendChild(levelTd);
            tr.appendChild(cell(row.msg, 'col-msg'));
            tr.appendChild(cell(`${row.sourceFile}:${row.sourceLine}`, 'col-source'));
            tr.addEventListener('click', () => toggleExpandRow(tr, row));
            tbody.appendChild(tr);
        }
    } catch (err) {
        statusEl.textContent = `Search failed: ${err}`;
    }
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
        const raw = await GetRawLine(currentBucket, row.fileHash);
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

async function onFindFiles() {
    const prefix = el('prefix-input').value;
    const tbody = el('files-tbody');
    clearChildren(tbody);
    el('load-summary').textContent = '';

    let files;
    try {
        files = await ListObjects(currentBucket, prefix);
    } catch (err) {
        el('load-summary').textContent = `Failed to list objects: ${err}`;
        return;
    }

    for (const f of files) {
        const tr = document.createElement('tr');

        const checkTd = document.createElement('td');
        const checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.value = f.name;
        checkbox.addEventListener('change', updateLoadButtonState);
        checkTd.appendChild(checkbox);

        tr.appendChild(checkTd);
        tr.appendChild(cell(f.name));
        tr.appendChild(cell(formatBytes(f.size)));
        tr.appendChild(cell(formatTime(f.lastModified)));
        tr.appendChild(cell(f.loaded ? 'Yes' : 'No'));
        tbody.appendChild(tr);
    }
    updateLoadButtonState();
}

function updateLoadButtonState() {
    const anyChecked = Array.from(el('files-tbody').querySelectorAll('input[type=checkbox]'))
        .some((cb) => cb.checked);
    el('load-files-btn').disabled = !anyChecked;
}

async function onLoadFiles() {
    const names = Array.from(el('files-tbody').querySelectorAll('input[type=checkbox]:checked'))
        .map((cb) => cb.value);
    if (names.length === 0) return;

    const summaryEl = el('load-summary');
    summaryEl.textContent = 'Loading…';

    try {
        const summaries = await LoadFiles(currentBucket, names);
        summaryEl.replaceChildren();
        for (const s of summaries) {
            const line = document.createElement('div');
            line.textContent = s.error
                ? `${s.name}: failed — ${s.error}`
                : `${s.name}: ${s.rowsInserted} row(s) inserted, ${s.linesSkipped} line(s) skipped`;
            summaryEl.appendChild(line);
        }
    } catch (err) {
        summaryEl.textContent = `Load failed: ${err}`;
    }

    await onFindFiles();
    await refreshLevelOptions();
}

async function onDeleteOlderThan() {
    const cutoffValue = el('retention-cutoff').value;
    const statusEl = el('retention-status');
    if (!cutoffValue) {
        statusEl.textContent = 'Pick a cutoff date first.';
        return;
    }
    if (!window.confirm(`Delete all rows in "${currentBucket}" older than ${cutoffValue}? This cannot be undone.`)) {
        return;
    }

    try {
        const deleted = await DeleteOlderThan(currentBucket, dateStartOfDayIsoOrNull(cutoffValue));
        statusEl.textContent = `Deleted ${deleted} row(s).`;
        await refreshLevelOptions();
    } catch (err) {
        statusEl.textContent = `Delete failed: ${err}`;
    }
}

function wireUpStaticControls() {
    el('bucket-select').addEventListener('change', onBucketChange);
    el('refresh-buckets-btn').addEventListener('click', refreshBuckets);
    el('search-btn').addEventListener('click', onSearch);
    el('query-input').addEventListener('keydown', (e) => { if (e.key === 'Enter') onSearch(); });
    el('find-files-btn').addEventListener('click', onFindFiles);
    el('load-files-btn').addEventListener('click', onLoadFiles);
    el('retention-btn').addEventListener('click', onDeleteOlderThan);
}

wireUpStaticControls();
initAuth();
