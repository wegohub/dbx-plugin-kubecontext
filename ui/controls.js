/* Plugin-owned controls: no dependency on DBX internals or system select menus. */
(() => {
  const controls = new Map();
  let opened;
  const closeSelects = () => opened?.close();
  function enhanceSelects(root = document) {
    for (const select of root.querySelectorAll('select')) {
      if (controls.has(select)) continue;
      const shell = document.createElement('span');
      shell.className = 'select-shell'; shell.dataset.for = select.id;
      const trigger = document.createElement('button');
      trigger.type = 'button'; trigger.className = 'select-trigger'; trigger.id = select.id + '-trigger';
      trigger.setAttribute('role', 'combobox'); trigger.setAttribute('aria-haspopup', 'listbox');
      trigger.setAttribute('aria-expanded', 'false');
      trigger.setAttribute('aria-label', select.getAttribute('aria-label') || select.title || '选择');
      const menu = document.createElement('div');
      menu.className = 'select-popover hidden';
      const search = document.createElement('input');
      search.type = 'search'; search.placeholder = '搜索选项…'; search.setAttribute('aria-label', '搜索' + trigger.getAttribute('aria-label'));
      const list = document.createElement('div');
      list.className = 'select-options'; list.id = select.id + '-options'; list.setAttribute('role', 'listbox');
      list.setAttribute('aria-label', trigger.getAttribute('aria-label'));
      trigger.setAttribute('aria-controls', list.id);
      menu.append(search, list); document.body.append(menu); shell.append(trigger); select.after(shell);
      select.hidden = true; select.tabIndex = -1; select.setAttribute('aria-hidden', 'true');
      let highlighted = -1, visible = [], typeahead = '', typedAt = 0;
      const control = {
        close() { menu.classList.add('hidden'); trigger.setAttribute('aria-expanded', 'false'); trigger.removeAttribute('aria-activedescendant'); if (opened === control) opened = undefined; },
        sync() {
          trigger.textContent = select.selectedOptions[0]?.textContent || '请选择…';
          trigger.title = select.selectedOptions[0]?.title || trigger.textContent;
          trigger.disabled = select.disabled;
          if (trigger.disabled) control.close();
          if (opened === control) render();
        },
      };
      controls.set(select, control);
      function highlight(index) {
        if (index < 0 || !visible.length) return;
        highlighted = index;
        for (const [i, option] of [...list.children].entries()) option.classList.toggle('highlighted', i === index);
        const option = list.children[index];
        if (option) { trigger.setAttribute('aria-activedescendant', option.id); option.scrollIntoView?.({block:'nearest'}); }
      }
      function choose(index) {
        const option = visible[index]; if (!option || option.disabled) return;
        const changed = select.value !== option.value;
        select.value = option.value; control.sync(); control.close(); trigger.focus();
        if (changed) select.dispatchEvent(new Event('change', {bubbles:true}));
      }
      function render() {
        const query = search.value.trim().toLowerCase();
        visible = [...select.options].filter(option => !option.hidden && (!query || option.textContent.toLowerCase().includes(query)));
        list.replaceChildren();
        for (const [index, option] of visible.entries()) {
          const row = document.createElement('div'); row.className = 'select-option'; row.id = select.id + '-option-' + index;
          row.setAttribute('role', 'option'); row.setAttribute('aria-selected', String(option.value === select.value)); row.setAttribute('aria-disabled', String(option.disabled));
          row.textContent = option.textContent; row.title = option.title || option.textContent;
          row.addEventListener('pointerdown', event => event.preventDefault());
          row.addEventListener('pointermove', () => { if (!option.disabled) highlight(index); });
          row.addEventListener('click', () => choose(index)); list.append(row);
        }
        if (!visible.length) { const empty = document.createElement('div'); empty.className = 'select-empty'; empty.textContent = '没有匹配的选项'; list.append(empty); }
        const current = visible.findIndex(option => option.value === select.value && !option.disabled);
        highlight(current >= 0 ? current : visible.findIndex(option => !option.disabled));
      }
      function open() {
        if (select.disabled) return;
        closeSelects(); opened = control; search.value = ''; search.hidden = select.options.length < 8;
        menu.classList.remove('hidden'); trigger.setAttribute('aria-expanded', 'true'); render();
        const rect = trigger.getBoundingClientRect();
        const width = Math.min(Math.max(rect.width, 200), Math.max(180, window.innerWidth - 16));
        menu.style.width = width + 'px'; menu.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - width - 8)) + 'px';
        const height = Math.min(menu.offsetHeight || 280, 320);
        menu.style.top = (window.innerHeight - rect.bottom > height + 10 ? rect.bottom + 5 : Math.max(8, rect.top - height - 5)) + 'px';
        if (!search.hidden) search.focus();
      }
      trigger.addEventListener('click', () => opened === control ? control.close() : open());
      function keydown(event) {
        const isOpen = opened === control;
        if (event.key === 'Escape' && isOpen) { event.preventDefault(); event.stopPropagation(); control.close(); trigger.focus(); return; }
        if (event.key === 'Tab') { control.close(); return; }
        if (['ArrowDown','ArrowUp','Home','End'].includes(event.key)) {
          event.preventDefault(); if (!isOpen) { open(); return; }
          const step = event.key === 'ArrowUp' || event.key === 'End' ? -1 : 1;
          let index = event.key === 'Home' ? 0 : event.key === 'End' ? visible.length - 1 : highlighted + step;
          while (index >= 0 && index < visible.length && visible[index].disabled) index += step;
          if (index >= 0 && index < visible.length) highlight(index);
          return;
        }
        if (event.key === 'Enter' || (event.key === ' ' && event.target === trigger)) {
          event.preventDefault(); isOpen ? choose(highlighted) : open(); return;
        }
        if (event.target === trigger && event.key.length === 1 && !event.metaKey && !event.ctrlKey) {
          event.preventDefault(); if (!isOpen) open();
          typeahead = Date.now() - typedAt < 600 ? typeahead + event.key : event.key; typedAt = Date.now();
          const index = visible.findIndex(option => !option.disabled && option.textContent.toLowerCase().startsWith(typeahead.toLowerCase()));
          if (index >= 0) highlight(index);
        }
      }
      trigger.addEventListener('keydown', keydown); search.addEventListener('keydown', keydown); search.addEventListener('input', render);
      select.addEventListener('change', control.sync);
      new MutationObserver(control.sync).observe(select, {childList:true,subtree:true,attributes:true,attributeFilter:['disabled','selected','label']});
      document.addEventListener('pointerdown', event => { if (opened === control && !shell.contains(event.target) && !menu.contains(event.target)) control.close(); });
      control.sync();
    }
  }
  window.addEventListener('resize', closeSelects);
  document.addEventListener('scroll', event => { if (opened && !event.target.closest?.('.select-popover')) closeSelects(); }, true);

  function bindDrawer({pane, container, handle, initialWidth, save}) {
    let width = Number(initialWidth) || 0, dragging;
    function bounds() {
      const available = container.getBoundingClientRect().width || window.innerWidth;
      const max = Math.max(180, available - (available > 760 ? 220 : 24));
      return {min:Math.min(320,max), max, available};
    }
    function resize(value) {
      const range = bounds(); width = Math.round(Math.max(range.min, Math.min(range.max, value)));
      pane.style.width = width + 'px';
      handle.setAttribute('aria-valuemin', range.min); handle.setAttribute('aria-valuemax', range.max); handle.setAttribute('aria-valuenow', width);
      return width;
    }
    function persist() { Promise.resolve(save?.(width)).catch(() => {}); }
    function open() { pane.classList.remove('hidden'); const {available} = bounds(); resize(width || available * (available > 760 ? .52 : .85)); }
    handle.addEventListener('pointerdown', event => {
      if (event.button !== 0 || pane.classList.contains('hidden')) return;
      event.preventDefault(); dragging = {id:event.pointerId, x:event.clientX, width};
      handle.setPointerCapture?.(event.pointerId); document.body.classList.add('resizing');
    });
    window.addEventListener('pointermove', event => { if (dragging && event.pointerId === dragging.id) resize(dragging.width + dragging.x - event.clientX); });
    function finish(event) {
      if (!dragging || event.pointerId !== dragging.id) return;
      dragging = null; document.body.classList.remove('resizing'); persist();
    }
    window.addEventListener('pointerup', finish); window.addEventListener('pointercancel', finish);
    handle.addEventListener('keydown', event => {
      const {min,max} = bounds(), step = event.shiftKey ? 48 : 16;
      if (!['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) return;
      event.preventDefault(); resize(event.key === 'Home' ? min : event.key === 'End' ? max : width + (event.key === 'ArrowLeft' ? step : -step)); persist();
    });
    handle.addEventListener('dblclick', () => { const {available} = bounds(); resize(available * (available > 760 ? .52 : .85)); persist(); });
    new ResizeObserver(() => { if (!pane.classList.contains('hidden')) resize(width); }).observe(container);
    return {open, resize};
  }
  function bindTooltips() {
    const tip = document.createElement('div'); tip.className='kc-tooltip hidden';tip.id='kc-tooltip';tip.setAttribute('role','tooltip');document.body.append(tip);
    let target=null;
    function hide() {target?.removeAttribute('aria-describedby');target=null;tip.classList.add('hidden');}
    function show(event) {
      const node=event.target.closest?.('[data-tooltip], [title]');
      if(!node || node===target)return;
      const text=node.dataset.tooltip || node.getAttribute('title');if(!text)return;
      hide();target=node;if(node.hasAttribute('title')){node.dataset.tooltip=text;node.removeAttribute('title');}tip.textContent=text;tip.classList.remove('hidden');node.setAttribute('aria-describedby',tip.id);
      const rect=node.getBoundingClientRect(),bounds=tip.getBoundingClientRect();
      tip.style.left=Math.max(8,Math.min(rect.left,window.innerWidth-bounds.width-8))+'px';
      tip.style.top=(rect.bottom+bounds.height+12<window.innerHeight?rect.bottom+6:Math.max(8,rect.top-bounds.height-6))+'px';
    }
    document.addEventListener('pointerover',show);document.addEventListener('focusin',show);
    document.addEventListener('pointerout',event=>{if(target&&!target.contains(event.relatedTarget))hide();});
    document.addEventListener('focusout',hide);document.addEventListener('pointerdown',hide);
    document.addEventListener('keydown',event=>{if(event.key==='Escape')hide();});
    document.addEventListener('scroll',hide,true);window.addEventListener('resize',hide);
    return {hide};
  }
  window.KubeControls = {enhanceSelects, closeSelects, bindDrawer, bindTooltips};
})();
