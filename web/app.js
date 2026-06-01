const state = {
  nodes: [],
  files: [],
  selectedAvailabilityID: ""
};

const els = {
  refreshBtn: document.querySelector("#refreshBtn"),
  nodesBody: document.querySelector("#nodesBody"),
  filesBody: document.querySelector("#filesBody"),
  filesHint: document.querySelector("#filesHint"),
  detailsTitle: document.querySelector("#detailsTitle"),
  availabilitySummary: document.querySelector("#availabilitySummary"),
  chunkMap: document.querySelector("#chunkMap"),
  manifestView: document.querySelector("#manifestView"),
  uploadForm: document.querySelector("#uploadForm"),
  fileInput: document.querySelector("#fileInput"),
  fileLabel: document.querySelector("#fileLabel"),
  backupName: document.querySelector("#backupName"),
  chunkSize: document.querySelector("#chunkSize"),
  replication: document.querySelector("#replication"),
  retention: document.querySelector("#retention"),
  uploadStatus: document.querySelector("#uploadStatus"),
  uploadProgress: document.querySelector("#uploadProgress"),
  metricNodes: document.querySelector("#metricNodes"),
  metricFree: document.querySelector("#metricFree"),
  metricUsed: document.querySelector("#metricUsed"),
  metricFiles: document.querySelector("#metricFiles"),
  metricDedup: document.querySelector("#metricDedup")
};

const TOKEN_KEY = "backupAuthToken";

function getToken() {
  let token = sessionStorage.getItem(TOKEN_KEY);
  if (!token) {
    token = (window.prompt("Podaj token dostepu (BACKUP_AUTH_TOKEN):") || "").trim();
    if (token) {
      sessionStorage.setItem(TOKEN_KEY, token);
    }
  }
  return token || "";
}

function clearToken() {
  sessionStorage.removeItem(TOKEN_KEY);
}

// authFetch dokleja token do kazdego zadania, a przy 401 czysci go i pyta o nowy raz.
async function authFetch(url, options = {}) {
  const opts = { ...options };
  opts.headers = new Headers(options.headers || {});
  opts.headers.set("Authorization", `Bearer ${getToken()}`);
  let response = await fetch(url, opts);
  if (response.status === 401) {
    clearToken();
    const retryToken = getToken();
    if (retryToken) {
      opts.headers.set("Authorization", `Bearer ${retryToken}`);
      response = await fetch(url, opts);
    }
  }
  return response;
}

els.refreshBtn.addEventListener("click", refresh);
els.uploadForm.addEventListener("submit", uploadBackup);
els.fileInput.addEventListener("change", () => {
  const file = els.fileInput.files[0];
  els.fileLabel.textContent = file ? `${file.name} (${formatBytes(file.size)})` : "Wybierz plik";
});

refresh();
setInterval(refresh, 5000);

async function refresh() {
  try {
    const [nodes, files] = await Promise.all([
      fetchJSON("/nodes"),
      fetchJSON("/files")
    ]);
    state.nodes = nodes;
    state.files = files;
    renderMetrics();
    renderNodes();
    renderFiles();
  } catch (err) {
    setStatus(`Blad odswiezania: ${err.message}`, "error");
  }
}

function renderMetrics() {
  const totalFree = state.nodes.reduce((sum, node) => sum + node.free, 0);
  const totalCapacity = state.nodes.reduce((sum, node) => sum + node.capacity, 0);
  const totalLogical = state.files.reduce((sum, file) => sum + (file.size || 0), 0);
  const totalDedup = state.files.reduce((sum, file) => sum + (file.dedup_reused_bytes || 0), 0);
  els.metricNodes.textContent = String(state.nodes.filter((node) => node.status === "online").length);
  els.metricFree.textContent = formatBytes(totalFree);
  els.metricUsed.textContent = formatBytes(Math.max(totalCapacity - totalFree, 0));
  els.metricFiles.textContent = String(state.files.length);
  els.metricDedup.textContent = `${formatBytes(totalDedup)} (${formatPercent(ratio(totalDedup, totalLogical))})`;
}

function renderNodes() {
  if (state.nodes.length === 0) {
    els.nodesBody.innerHTML = `<tr><td class="empty" colspan="7">Brak podlaczonych klientow.</td></tr>`;
    return;
  }

  els.nodesBody.innerHTML = state.nodes.map((node) => {
    const used = Math.max(node.capacity - node.free, 0);
    const removeButton = node.status === "offline"
      ? `<button class="secondary danger" type="button" data-delete-node="${escapeHTML(node.id)}" data-purge-storage="false">Usun metadane</button>`
      : `<button class="secondary danger" type="button" data-delete-node="${escapeHTML(node.id)}" data-purge-storage="true">Wyczysc i usun</button>`;
    return `
      <tr>
        <td>${escapeHTML(node.id)}</td>
        <td>${escapeHTML(node.address)}</td>
        <td><span class="badge">${escapeHTML(node.status)}</span></td>
        <td>${formatBytes(node.free)}</td>
        <td>${formatBytes(used)}</td>
        <td>${formatDate(node.last_seen)}</td>
        <td>${removeButton}</td>
      </tr>
    `;
  }).join("");

  els.nodesBody.querySelectorAll("[data-delete-node]").forEach((button) => {
    button.addEventListener("click", () => deleteNode(button.dataset.deleteNode, button.dataset.purgeStorage === "true"));
  });
}

function renderFiles() {
  els.filesHint.textContent = state.files.length ? `${state.files.length} zapisanych` : "";
  if (state.files.length === 0) {
    els.filesBody.innerHTML = `<tr><td class="empty" colspan="9">Nie ma jeszcze backupow.</td></tr>`;
    return;
  }

  els.filesBody.innerHTML = state.files.map((file) => `
    <tr>
      <td>${escapeHTML(file.name)}</td>
      <td>${escapeHTML(file.backup_name || file.name)}</td>
      <td>v${file.version || 1}</td>
      <td>${formatBytes(file.size)}</td>
      <td title="Oszczedzone ${formatBytes(file.dedup_reused_bytes || 0)}, nowe ${formatBytes(file.dedup_new_bytes || 0)}">${formatPercent(file.dedup_ratio || 0)}</td>
      <td>
        <div class="replication-control">
          <input type="number" min="1" max="10" value="${file.replication || 3}" data-replication-input="${escapeHTML(file.id)}">
          <button class="secondary" type="button" data-replication-save="${escapeHTML(file.id)}">Zapisz</button>
        </div>
      </td>
      <td>${file.chunks.length}</td>
      <td>${escapeHTML(file.id)}</td>
      <td>
        <div class="actions">
          <button class="secondary" type="button" data-manifest="${escapeHTML(file.id)}">Manifest</button>
          <button class="secondary" type="button" data-availability="${escapeHTML(file.id)}">Chunki</button>
          <button class="secondary" type="button" data-download="${escapeHTML(file.id)}" data-download-name="${escapeHTML(file.name)}">Restore</button>
          <button class="secondary danger" type="button" data-delete="${escapeHTML(file.id)}">Usun</button>
        </div>
      </td>
    </tr>
  `).join("");

  els.filesBody.querySelectorAll("[data-manifest]").forEach((button) => {
    button.addEventListener("click", () => showManifest(button.dataset.manifest));
  });
  els.filesBody.querySelectorAll("[data-availability]").forEach((button) => {
    button.addEventListener("click", () => showAvailability(button.dataset.availability));
  });
  els.filesBody.querySelectorAll("[data-delete]").forEach((button) => {
    button.addEventListener("click", () => deleteBackup(button.dataset.delete));
  });
  els.filesBody.querySelectorAll("[data-download]").forEach((button) => {
    button.addEventListener("click", () => downloadFile(button.dataset.download, button.dataset.downloadName));
  });
  els.filesBody.querySelectorAll("[data-replication-save]").forEach((button) => {
    button.addEventListener("click", () => {
      const input = els.filesBody.querySelector(`[data-replication-input="${cssEscape(button.dataset.replicationSave)}"]`);
      updateReplication(button.dataset.replicationSave, Number(input.value));
    });
  });
}

async function showManifest(id) {
  try {
    const manifest = await fetchJSON(`/files/${encodeURIComponent(id)}/manifest`);
    els.detailsTitle.textContent = "Manifest";
    els.availabilitySummary.innerHTML = "";
    els.chunkMap.innerHTML = "";
    els.manifestView.textContent = JSON.stringify(manifest, null, 2);
  } catch (err) {
    els.manifestView.textContent = `Blad pobierania manifestu: ${err.message}`;
  }
}

async function showAvailability(id) {
  try {
    state.selectedAvailabilityID = id;
    const availability = await fetchJSON(`/files/${encodeURIComponent(id)}/availability`);
    const counts = availability.chunks.reduce((acc, chunk) => {
      acc[chunk.status] = (acc[chunk.status] || 0) + 1;
      return acc;
    }, {});

    els.detailsTitle.textContent = `Chunki: ${availability.name}`;
    els.availabilitySummary.innerHTML = `
      <span class="availability-pill healthy">OK ${counts.healthy || 0}</span>
      <span class="availability-pill degraded">Malo kopii ${counts.degraded || 0}</span>
      <span class="availability-pill missing">Brak ${counts.missing || 0}</span>
      <span class="availability-pill extra">Nadmiar ${counts.extra || 0}</span>
    `;
    els.chunkMap.innerHTML = availability.chunks.map((chunk) => `
      <button
        class="chunk-cell ${escapeHTML(chunk.status)}"
        type="button"
        title="#${chunk.index} ${chunk.active}/${chunk.desired} active copies"
        data-index="${chunk.index}">
      </button>
    `).join("");

    els.chunkMap.querySelectorAll("[data-index]").forEach((button) => {
      button.addEventListener("click", () => {
        const chunk = availability.chunks[Number(button.dataset.index)];
        renderChunkDetails(availability, chunk);
      });
    });

    els.manifestView.textContent = renderAvailabilityText(availability);
  } catch (err) {
    els.detailsTitle.textContent = "Chunki";
    els.availabilitySummary.innerHTML = "";
    els.chunkMap.innerHTML = "";
    els.manifestView.textContent = `Blad pobierania dostepnosci: ${err.message}`;
  }
}

async function updateReplication(id, replication) {
  if (!Number.isInteger(replication) || replication < 1) {
    setStatus("Liczba kopii musi byc dodatnia liczba calkowita.", "error");
    return;
  }
  try {
    const response = await authFetch(`/files/${encodeURIComponent(id)}/replication`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ replication })
    });
    const body = await response.json();
    if (!response.ok) {
      throw new Error(body.error || response.statusText);
    }
    setStatus(`Zmieniono liczbe kopii dla ${id} na ${body.replication}.`, "ok");
    await refresh();
    await showAvailability(id);
  } catch (err) {
    setStatus(`Zmiana liczby kopii nieudana: ${err.message}`, "error");
  }
}

function renderChunkDetails(file, chunk) {
  const lines = [
    `${file.name}`,
    `chunk #${chunk.index}`,
    `hash: ${chunk.hash}`,
    `size: ${formatBytes(chunk.size)}`,
    `status: ${chunk.status}`,
    `active copies: ${chunk.active}/${chunk.desired}`,
    `known locations: ${chunk.total}`,
    "",
    "locations:"
  ];

  if (chunk.locations.length === 0) {
    lines.push("  none");
  } else {
    chunk.locations.forEach((node) => {
      lines.push(`  ${node.id} | ${node.status} | ${node.address} | free ${formatBytes(node.free)}`);
    });
  }
  els.manifestView.textContent = lines.join("\n");
}

function renderAvailabilityText(file) {
  const lines = [
    `${file.name}`,
    `backup_name: ${file.backup_name || file.name}`,
    `version: ${file.version || 1}`,
    `file_id: ${file.file_id}`,
    `created_at: ${formatDate(file.created_at)}`,
    `size: ${formatBytes(file.size)}`,
    `dedup_saved: ${formatPercent(file.dedup_ratio || 0)} (${formatBytes(file.dedup_reused_bytes || 0)})`,
    `dedup_new: ${formatBytes(file.dedup_new_bytes || 0)}`,
    `chunk_size: ${formatBytes(file.chunk_size)}`,
    `replication: ${file.replication}`,
    `chunks: ${file.chunks.length}`,
    "",
    "index | status | active/desired | locations | hash"
  ];

  file.chunks.forEach((chunk) => {
    lines.push(`${chunk.index} | ${chunk.status} | ${chunk.active}/${chunk.desired} | ${chunk.total} | ${chunk.hash}`);
  });
  return lines.join("\n");
}

async function uploadBackup(event) {
  event.preventDefault();
  const file = els.fileInput.files[0];
  if (!file) {
    setStatus("Wybierz plik do backupu.", "error");
    return;
  }

  const form = new FormData();
  form.append("file", file);

  const params = new URLSearchParams({
    chunk_size: els.chunkSize.value,
    replication: els.replication.value,
    retention: els.retention.value
  });
  const backupName = els.backupName.value.trim();
  if (backupName) {
    params.set("backup_name", backupName);
  }

  setStatus("Wysylanie pliku do serwera...", "");
  els.uploadProgress.style.width = "20%";

  try {
    const response = await authFetch(`/backups?${params.toString()}`, {
      method: "POST",
      body: form
    });
    const body = await response.json();
    if (!response.ok) {
      throw new Error(body.error || response.statusText);
    }
    els.uploadProgress.style.width = "100%";
    const deletedText = body.retention_deleted && body.retention_deleted.length
      ? `, retencja usunela: ${body.retention_deleted.length}`
      : "";
    setStatus(`Backup zapisany: ${body.backup_name || body.name} v${body.version || 1}, dedup ${formatPercent(body.dedup_ratio || 0)}${deletedText}`, "ok");
    els.uploadForm.reset();
    els.fileLabel.textContent = "Wybierz plik";
    await refresh();
    await showManifest(body.id);
    setTimeout(() => {
      els.uploadProgress.style.width = "0";
    }, 900);
  } catch (err) {
    els.uploadProgress.style.width = "0";
    setStatus(`Backup nieudany: ${err.message}`, "error");
  }
}

async function downloadFile(id, name) {
  try {
    setStatus(`Pobieranie ${name}...`, "");
    const response = await authFetch(`/files/${encodeURIComponent(id)}/download`);
    if (!response.ok) {
      let message = response.statusText;
      try {
        message = (await response.json()).error || message;
      } catch (_) {
        // odpowiedz nie byla JSON-em, zostaje statusText
      }
      throw new Error(message);
    }
    const blob = await response.blob();
    const objectURL = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = objectURL;
    link.download = name || id;
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(objectURL);
    setStatus(`Pobrano ${name}.`, "ok");
  } catch (err) {
    setStatus(`Pobieranie nieudane: ${err.message}`, "error");
  }
}

async function deleteBackup(id) {
  if (!confirm(`Usunac backup ${id} i nieuzywane chunki z node'ow?`)) {
    return;
  }
  try {
    const response = await authFetch(`/files/${encodeURIComponent(id)}`, { method: "DELETE" });
    const body = await response.json();
    if (!response.ok) {
      throw new Error(body.error || response.statusText);
    }
    setStatus(`Backup usuniety: ${id}, chunki: ${body.deleted_chunks}`, "ok");
    els.detailsTitle.textContent = "Szczegoly";
    els.availabilitySummary.innerHTML = "";
    els.chunkMap.innerHTML = "";
    els.manifestView.textContent = "Wybierz backup z listy.";
    await refresh();
  } catch (err) {
    setStatus(`Usuwanie nieudane: ${err.message}`, "error");
  }
}

async function deleteNode(id, purgeStorage) {
  const message = purgeStorage
    ? `Wyczyscic storage node'a ${id} i usunac go z klastra? Uzywaj tego tylko przy swiadomym wycofaniu dzialajacego node'a.`
    : `Usunac martwy node ${id} z klastra? Serwer usunie tylko metadane i lokalizacje chunkow z SQLite. Fizyczny katalog na tamtej maszynie trzeba wyczyscic lokalnie.`;
  if (!confirm(message)) {
    return;
  }
  try {
    const suffix = purgeStorage ? "?purge=true" : "";
    const response = await authFetch(`/nodes/${encodeURIComponent(id)}${suffix}`, { method: "DELETE" });
    const body = await response.json();
    if (!response.ok) {
      throw new Error(body.error || response.statusText);
    }
    const purgeText = body.purged_storage ? ", storage wyczyszczony" : "";
    setStatus(`Node usuniety: ${body.deleted_node}, lokalizacje: ${body.removed_locations}${purgeText}`, "ok");
    await refresh();
    if (state.selectedAvailabilityID) {
      await showAvailability(state.selectedAvailabilityID);
    }
  } catch (err) {
    setStatus(`Usuwanie node'a nieudane: ${err.message}`, "error");
  }
}

async function fetchJSON(url) {
  const response = await authFetch(url);
  const data = await response.json();
  if (!response.ok) {
    throw new Error(data.error || response.statusText);
  }
  return data;
}

function setStatus(message, kind) {
  els.uploadStatus.textContent = message;
  els.uploadStatus.dataset.kind = kind;
}

function formatBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return "0 B";
  }
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return `${value.toFixed(value >= 10 || unit === 0 ? 0 : 1)} ${units[unit]}`;
}

function formatDate(value) {
  if (!value) {
    return "-";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString();
}

function formatPercent(value) {
  if (!Number.isFinite(value) || value <= 0) {
    return "0%";
  }
  return `${(value * 100).toFixed(value >= 0.1 ? 1 : 2)}%`;
}

function ratio(part, total) {
  if (!Number.isFinite(part) || !Number.isFinite(total) || part <= 0 || total <= 0) {
    return 0;
  }
  return part / total;
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function cssEscape(value) {
  if (window.CSS && typeof window.CSS.escape === "function") {
    return window.CSS.escape(value);
  }
  return String(value).replaceAll('"', '\\"');
}
