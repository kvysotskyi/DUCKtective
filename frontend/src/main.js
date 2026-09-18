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
let lastFiles = [];

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

function cell(text) {
    const td = document.createElement('td');
    td.textContent = text ?? '';
    return td;
}

async function initAuth() {
    const banner = el('auth-banner');
    try {
        const status = await CheckAuth();
        if (status.available) {
            banner.className = 'banner ok';
            banner.textContent = `Signed in as ${status.account || '(unknown account)'} — project ${status.projectId || '(none set)'}`;
            await refreshBuckets();
        } else {
            banner.className = 'banner error';
            banner.textContent = status.message;
            addRetryButton(banner);
        }
    } catch (err) {
        banner.className = 'banner error';
        banner.textContent = `Auth check failed: ${err}`;
    }
}

function addRetryButton(banner) {
    const btn = document.createElement('button');
    btn.textContent = 'Retry';
    btn.addEventListener('click', initAuth);
    banner.appendChild(document.createElement('br'));
    banner.appendChild(btn);
}

async function refreshBuckets() {
    const select = el('bucket-select');
    const status = el('bucket-status');
    clearChildren(select);
    select.appendChild(new Option('Select a bucket…', ''));

    try {
        const buckets = await ListBuckets();
        for (const b of buckets) select.appendChild(new Option(b.name, b.name));
        status.textContent = `${buckets.length} bucket(s) in active project`;
    } catch (err) {
        status.textContent = `Could not list buckets (${err}) — showing already-loaded buckets only`;
        try {
            const loaded = await ListLoadedBuckets();
            for (const name of loaded) select.appendChild(new Option(name, name));
        } catch {
            // nothing to fall back to either — leave the empty select
        }
    }
}

async function onBucketChange() {
    currentBucket = el('bucket-select').value;
    const hasBucket = Boolean(currentBucket);
    el('files-section').hidden = !hasBucket;
    el('search-section').hidden = !hasBucket;
    el('retention-section').hidden = !hasBucket;
    if (!hasBucket) return;

    el('files-table').querySelector('tbody').replaceChildren();
    el('results-table').querySelector('tbody').replaceChildren();
    await refreshLevelOptions();
}

async function refreshLevelOptions() {
    const select = el('filter-level');
    clearChildren(select);
    select.appendChild(new Option('Any', ''));
    try {
        const levels = await DistinctLevels(currentBucket);
        for (const l of levels) select.appendChild(new Option(l, l));
    } catch {
        // no rows loaded yet for this bucket — leave just "Any"
    }
}

async function onFindFiles() {
    const prefix = el('prefix-input').value;
    const tbody = el('files-table').querySelector('tbody');
    clearChildren(tbody);
    el('load-summary').textContent = '';

    try {
        lastFiles = await ListObjects(currentBucket, prefix);
    } catch (err) {
        lastFiles = [];
        el('load-summary').textContent = `Failed to list objects: ${err}`;
        return;
    }

    for (const f of lastFiles) {
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
    const anyChecked = Array.from(el('files-table').querySelectorAll('tbody input[type=checkbox]'))
        .some((cb) => cb.checked);
    el('load-files-btn').disabled = !anyChecked;
}

async function onLoadFiles() {
    const names = Array.from(el('files-table').querySelectorAll('tbody input[type=checkbox]:checked'))
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

function buildFilters() {
    return {
        timeFrom: isoOrNull(el('filter-from').value),
        timeTo: isoOrNull(el('filter-to').value),
        level: el('filter-level').value,
        msg: el('filter-msg').value,
        topic: el('filter-topic').value,
        accession: el('filter-accession').value,
        studyUid: el('filter-studyuid').value,
    };
}

async function onSearch() {
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
            tr.appendChild(cell(formatTime(row.effectiveTs)));
            tr.appendChild(cell(row.level));
            tr.appendChild(cell(row.msg));
            tr.appendChild(cell(row.topic));
            tr.appendChild(cell(row.accession));
            tr.appendChild(cell(row.studyUid));
            tr.appendChild(cell(`${row.sourceFile}:${row.sourceLine}`));
            tr.addEventListener('click', () => showRawLine(row.fileHash));
            tbody.appendChild(tr);
        }
    } catch (err) {
        statusEl.textContent = `Search failed: ${err}`;
    }
}

async function showRawLine(fileHash) {
    const modal = el('raw-modal');
    const body = el('raw-modal-body');
    try {
        const raw = await GetRawLine(currentBucket, fileHash);
        try {
            body.textContent = JSON.stringify(JSON.parse(raw), null, 2);
        } catch {
            body.textContent = raw;
        }
    } catch (err) {
        body.textContent = `Failed to load raw line: ${err}`;
    }
    modal.hidden = false;
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
    el('find-files-btn').addEventListener('click', onFindFiles);
    el('load-files-btn').addEventListener('click', onLoadFiles);
    el('search-btn').addEventListener('click', onSearch);
    el('retention-btn').addEventListener('click', onDeleteOlderThan);
    el('raw-modal-close').addEventListener('click', () => { el('raw-modal').hidden = true; });
    el('raw-modal').addEventListener('click', (e) => {
        if (e.target === el('raw-modal')) el('raw-modal').hidden = true;
    });
}

wireUpStaticControls();
initAuth();
